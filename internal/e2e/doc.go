// Package e2e holds end-to-end tests of the whole application: the real
// store, fetcher, parser, resolver, scheduler and web UI wired together as
// in cmd/feedreader, driven over HTTP the way htmx drives them, against a
// local fake feed site (no internet access).
package e2e
