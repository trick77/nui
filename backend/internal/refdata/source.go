package refdata

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/trick77/ship/kit/logging"
	"github.com/trick77/ship/kit/run"

	"github.com/trick77/trakk/backend/internal/alert"
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
	// Every is the age a cached source is fetched again at, a day by default; Run looks every Every/rechecks.
	Every time.Duration
	Log   *slog.Logger
	// Report takes each refresh that fetched something (job.failed, refdata): a failed download or parse opens it,
	// the next refresh that gets everything through clears it.
	Report alert.Reporter
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
	if b, err := d.loadBots(d.path(crawlersFile), d.path(monitorFile)); err == nil {
		d.installBots(b)
	}
	if n, err := loadNetworks(d.path(networksFile)); err == nil {
		d.installNetworks(n)
	}
	if c, err := loadCountries(d.path(countriesFile)); err == nil {
		d.installCountries(c)
	}
}

func (d *Data) path(file string) string { return filepath.Join(d.o.Dir, file) }

func (d *Data) installBots(b *Bots) {
	d.bots.Store(b)
	d.o.Log.Info("Bot list loaded", "services", len(b.Providers))
	d.logRejected("bot list", b.Rejected)
	// Info, not Warn: an agent left out costs a name, never an address.
	if r := b.RejectedAgents; r.Count > 0 {
		d.o.Log.Info("Bot list agents left out: of no use, a name taken, another purpose or a second listing",
			"count", r.Count, "first", strings.Join(r.First, ", "))
	}
}

func (d *Data) installNetworks(n *Networks) {
	d.networks.Store(n)
	d.o.Log.Info("AS data loaded", "as", len(n.AS), "prefixes", n.table.Len())
	d.logRejected("AS data", n.Rejected)
}

func (d *Data) installCountries(c *Countries) {
	d.countries.Store(c)
	d.o.Log.Info("Country data loaded", "countries", len(c.Codes), "prefixes", c.table.Len())
	d.logRejected("country data", c.Rejected)
}

func (d *Data) logRejected(what string, r Rejected) {
	if r.Count > 0 {
		d.o.Log.Warn("Reference data rejected as implausibly broad or unreadable", "data", what, "count", r.Count,
			"first", strings.Join(r.First, ", "))
	}
}

// rechecks is how often within Options.Every Run looks again: every 15 minutes of a day.
const rechecks = 96

// Run refreshes every source whose cache is missing or older than Options.Every, now and at each recheck until ctx
// ends. A failed refresh logs and keeps the last good data: the network never stops the service. It is tried again
// at the next recheck, not an interval later: a download failing at a first start left a day's events without
// network and country. Each further failure doubles the wait, up to the interval: a tarball that arrives and does
// not parse is not pulled 96 times a day.
func (d *Data) Run(ctx context.Context) {
	wait := d.o.Every / rechecks
	for {
		if d.refresh(ctx) {
			wait = d.o.Every / rechecks
		}
		if !run.Sleep(ctx, wait) {
			return
		}
		wait = min(2*wait, d.o.Every)
	}
}

// Refresh fetches the sources whose cache is older than Options.Every. Every download is parsed before it replaces
// the cached copy: one that does not parse (an error page served with 200, a truncated file) is discarded, and the
// last good copy stays on disk and in memory.
func (d *Data) Refresh(ctx context.Context) { d.refresh(ctx) }

// refresh is Refresh, and whether nothing it tried failed.
func (d *Data) refresh(ctx context.Context) (ok bool) {
	tried := false
	var errs []error
	warn := func(what string, err error) {
		tried, errs = true, append(errs, err)
		d.warn(what, err)
	}
	defer func() {
		ok = errors.Join(errs...) == nil
		if tried && ctx.Err() == nil {
			alert.Fail(d.o.Report, alert.JobRefdata, errors.Join(errs...))
		}
	}()
	if d.stale(crawlersFile) || d.stale(monitorFile) || d.Bots() == nil {
		warn("bot list", d.refreshBots(ctx))
	}
	for _, src := range []struct {
		name, url, file string
		install         func(tmp string) (func(), error)
	}{
		{"AS data", d.o.Networks, networksFile, func(tmp string) (func(), error) {
			n, err := loadNetworks(tmp)
			return func() { n.Updated = modTime(d.path(networksFile)); d.installNetworks(n) }, err
		}},
		{"country data", d.o.Countries, countriesFile, func(tmp string) (func(), error) {
			c, err := loadCountries(tmp)
			return func() { c.Updated = modTime(d.path(countriesFile)); d.installCountries(c) }, err
		}},
	} {
		if !d.stale(src.file) && d.loaded(src.file) {
			continue
		}
		warn(src.name, d.refreshOne(ctx, src.url, src.file, src.install))
	}
	return true
}

// refreshOne downloads one source, parses the download and only then moves it in place and installs it.
func (d *Data) refreshOne(ctx context.Context, url, file string, parse func(tmp string) (func(), error)) error {
	tmp, err := d.download(ctx, url, file)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp) }()
	install, err := parse(tmp)
	if err != nil {
		return fmt.Errorf("%s: %w", url, err)
	}
	if err := os.Rename(tmp, d.path(file)); err != nil {
		return err
	}
	install()
	return nil
}

// refreshBots downloads both bot lists and replaces the cached pair only when both parse.
func (d *Data) refreshBots(ctx context.Context) error {
	ct, err := d.download(ctx, d.o.Crawlers, crawlersFile)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(ct) }()
	mt, err := d.download(ctx, d.o.Monitoring, monitorFile)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(mt) }()
	b, err := d.loadBots(ct, mt)
	if err != nil {
		return err
	}
	if err := os.Rename(mt, d.path(monitorFile)); err != nil {
		return err
	}
	if err := os.Rename(ct, d.path(crawlersFile)); err != nil {
		return err
	}
	b.Updated = modTime(d.path(crawlersFile))
	d.installBots(b)
	return nil
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

// download fetches url into a temp file beside file and returns its path; the caller moves it in place once it
// parses, or removes it.
func (d *Data) download(ctx context.Context, url, file string) (string, error) {
	if err := os.MkdirAll(d.o.Dir, 0o750); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "trakk-refdata")
	resp, err := d.o.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	tmp, err := os.CreateTemp(d.o.Dir, file+".*.tmp")
	if err != nil {
		return "", err
	}
	n, err := io.Copy(tmp, io.LimitReader(resp.Body, maxDownload+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > maxDownload {
		err = fmt.Errorf("GET %s: over %d bytes", url, maxDownload)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
}

// loadBots parses the two bot lists at the given paths, dated by the crawler list's file.
func (d *Data) loadBots(crawlers, monitoring string) (*Bots, error) {
	c, err := os.ReadFile(crawlers) //nolint:gosec // a cache file under the data dir
	if err != nil {
		return nil, err
	}
	m, err := os.ReadFile(monitoring) //nolint:gosec // a cache file under the data dir
	if err != nil {
		return nil, err
	}
	b, err := parseBots(c, m)
	if err != nil {
		return nil, err
	}
	b.Updated = modTime(crawlers)
	return b, nil
}

func loadNetworks(p string) (*Networks, error) {
	n, err := loadTar(p, parseNetworks)
	if err == nil {
		n.Updated = modTime(p)
	}
	return n, err
}

func loadCountries(p string) (*Countries, error) {
	c, err := loadTar(p, parseCountries)
	if err == nil {
		c.Updated = modTime(p)
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
