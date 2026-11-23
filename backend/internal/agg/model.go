package agg

// raw is one event row as the builder reads it.
//
// Table: event
type raw struct {
	ts                                                    int64
	source, kind, name, visitor, sid, ip, ipKey, ua, tmpl string
	refHost, country                                      string
	status                                                int
	asn                                                   int64
	cls                                                   int
}
