package refdata

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/trick77/ship/kit/logging"
	"github.com/trick77/ship/kit/run"
)

// The ipverse sources, as lens fetches them.
const (
	CrawlersURL    = "https://raw.githubusercontent.com/ipverse/bot-ip-blocks/master/crawlers.json"
	MonitoringURL  = "https://raw.githubusercontent.com/ipverse/bot-ip-blocks/master/monitoring.json"
	ASIPBlocksURL  = "https://github.com/ipverse/as-ip-blocks/releases/download/latest/as-ip-blocks.tar.gz"
	GeoIPBlocksURL = "https://github.com/ipverse/geo-ip-blocks/releases/download/latest/geo-ip-blocks.tar.gz"
)

// maxDownload bounds one download.
const maxDownload = 256 << 20

// The cached files in Options.Dir.
const (
	crawlersFile  = "crawlers.json"
	monitorFile   = "monitoring.json"
	networksFile  = "as-ip-blocks.tar.gz"
	countriesFile = "geo-ip-blocks.tar.gz"
)

// Options configures Data.
type Options struct {
	// Dir caches the downloads; the data dir's ref/ in production.
	Dir string
	// Client fetches; nil is a client with a two-minute timeout.
	Client *http.Client
	// URLs override the sources (tests); empty fields keep the defaults.
	Crawlers, Monitoring, Networks, Countries string
	// Every is the refresh interval, a day by default.
	Every time.Duration
	Log   *slog.Logger
}

// Data is the current reference data. The zero value has none and answers every lookup with "unknown".
type Data struct {
	o         Options
	bots      atomic.Pointer[Bots]
	networks  atomic.Pointer[Networks]
	countries atomic.Pointer[Countries]
}

// New returns Data for o; Load and Run fill it.
func New(o Options) *Data {
	if o.Client == nil {
		o.Client = &http.Client{Timeout: 2 * time.Minute}
	}
	if o.Crawlers == "" {
		o.Crawlers = CrawlersURL
	}
	if o.Monitoring == "" {
		o.Monitoring = MonitoringURL
	}
	if o.Networks == "" {
		o.Networks = ASIPBlocksURL
	}
	if o.Countries == "" {
		o.Countries = GeoIPBlocksURL
	}
	if o.Every <= 0 {
		o.Every = 24 * time.Hour
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	return &Data{o: o}
}

// Bots is the current bot list, nil before the first load.
func (d *Data) Bots() *Bots { return d.bots.Load() }

// Networks is the current AS data, nil before the first load.
func (d *Data) Networks() *Networks { return d.networks.Load() }

// Countries is the current country data, nil before the first load.
func (d *Data) Countries() *Countries { return d.countries.Load() }

// SetBots installs a bot list (tests, and the loaders).
func (d *Data) SetBots(b *Bots) { d.bots.Store(b) }

// LoadCached loads whatever the cache dir holds, each source on its own: a restart without network still classifies
// and enriches with the last good copy.
func (d *Data) LoadCached() {
	if b, err := d.loadBots(); err == nil {
		d.bots.Store(b)
	}
	if n, err := d.loadNetworks(); err == nil {
		d.networks.Store(n)
	}
	if c, err := d.loadCountries(); err == nil {
		d.countries.Store(c)
	}
}

// Run refreshes every source once now when its cache is missing or stale, then every Options.Every, until ctx
// ends. A failed refresh logs and keeps the last good data: the network never stops the service.
func (d *Data) Run(ctx context.Context) {
	for {
		d.Refresh(ctx)
		if !run.Sleep(ctx, d.o.Every) {
			return
		}
	}
}

// Refresh fetches the sources whose cache is older than Options.Every, parses each and installs it when it parses.
func (d *Data) Refresh(ctx context.Context) {
	if d.stale(crawlersFile) || d.stale(monitorFile) || d.Bots() == nil {
		err := d.fetch(ctx, d.o.Crawlers, crawlersFile, nil)
		if err == nil {
			err = d.fetch(ctx, d.o.Monitoring, monitorFile, nil)
		}
		if err == nil {
			var b *Bots
			if b, err = d.loadBots(); err == nil {
				d.bots.Store(b)
				d.o.Log.Info("Bot list loaded", "services", len(b.Providers))
			}
		}
		d.warn("bot list", err)
	}
	for _, src := range []struct {
		name, url, file string
		install         func() error
	}{
		{"AS data", d.o.Networks, networksFile, func() error {
			n, err := d.loadNetworks()
			if err == nil {
				d.networks.Store(n)
				d.o.Log.Info("AS data loaded", "ases", len(n.ASes), "prefixes", n.table.Len())
			}
			return err
		}},
		{"country data", d.o.Countries, countriesFile, func() error {
			c, err := d.loadCountries()
			if err == nil {
				d.countries.Store(c)
				d.o.Log.Info("Country data loaded", "countries", len(c.Codes), "prefixes", c.table.Len())
			}
			return err
		}},
	} {
		if !d.stale(src.file) && d.loaded(src.file) {
			continue
		}
		err := d.fetch(ctx, src.url, src.file, src.install)
		d.warn(src.name, err)
	}
}

func (d *Data) loaded(file string) bool {
	switch file {
	case networksFile:
		return d.Networks() != nil
	case countriesFile:
		return d.Countries() != nil
	}
	return true
}

func (d *Data) warn(what string, err error) {
	if err != nil {
		d.o.Log.Warn("Reference data refresh failed, keeping the last good copy", "data", what, logging.Err(err))
	}
}

// stale reports whether the cached file is missing or older than the interval.
func (d *Data) stale(file string) bool {
	st, err := os.Stat(filepath.Join(d.o.Dir, file))
	return err != nil || time.Since(st.ModTime()) >= d.o.Every
}

// fetch downloads url into a temp file beside file, checks it with install (when given) after moving it in place,
// and puts the previous copy back when the check fails.
func (d *Data) fetch(ctx context.Context, url, file string, install func() error) error {
	if err := os.MkdirAll(d.o.Dir, 0o750); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "nui-refdata")
	resp, err := d.o.Client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	dest := filepath.Join(d.o.Dir, file)
	tmp, err := os.CreateTemp(d.o.Dir, file+".*.tmp")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	n, err := io.Copy(tmp, io.LimitReader(resp.Body, maxDownload+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n > maxDownload {
		return fmt.Errorf("GET %s: over %d bytes", url, maxDownload)
	}
	prev := dest + ".prev"
	_ = os.Remove(prev)
	hadPrev := os.Rename(dest, prev) == nil
	if err := os.Rename(tmp.Name(), dest); err != nil {
		return err
	}
	if install != nil {
		if err := install(); err != nil {
			if hadPrev {
				_ = os.Rename(prev, dest)
			}
			return err
		}
	}
	_ = os.Remove(prev)
	return nil
}

func (d *Data) loadBots() (*Bots, error) {
	c, err := os.ReadFile(filepath.Join(d.o.Dir, crawlersFile))
	if err != nil {
		return nil, err
	}
	m, err := os.ReadFile(filepath.Join(d.o.Dir, monitorFile))
	if err != nil {
		return nil, err
	}
	b, err := parseBots(c, m)
	if err != nil {
		return nil, err
	}
	b.Updated = modTime(filepath.Join(d.o.Dir, crawlersFile))
	return b, nil
}

func (d *Data) loadNetworks() (*Networks, error) {
	n, err := loadTar(filepath.Join(d.o.Dir, networksFile), parseNetworks)
	if err == nil {
		n.Updated = modTime(filepath.Join(d.o.Dir, networksFile))
	}
	return n, err
}

func (d *Data) loadCountries() (*Countries, error) {
	c, err := loadTar(filepath.Join(d.o.Dir, countriesFile), parseCountries)
	if err == nil {
		c.Updated = modTime(filepath.Join(d.o.Dir, countriesFile))
	}
	return c, err
}

func loadTar[T any](p string, parse func(io.Reader) (*T, error)) (*T, error) {
	f, err := os.Open(p) //nolint:gosec // a cache file under the data dir
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return parse(f)
}

func modTime(p string) time.Time {
	st, err := os.Stat(p)
	if err != nil {
		return time.Time{}
	}
	return st.ModTime().UTC()
}
