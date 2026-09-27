// Package status is the Collector's health report (collector.md §2.5): the
// JSON the control socket returns for `GET /status`, and the text
// `agent-history status` prints from it.
package status

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// Report is the whole `status` report. Fields the reporting process can't
// know are nil: without a running service there is no last sync, pending
// count or excluded count.
type Report struct {
	Version     string `json:"version"`
	MachineID   string `json:"machine_id"`
	DisplayName string `json:"display_name"`
	HubURL      string `json:"hub_url"`

	// ServiceRunning is whether a Collector answered on the control socket.
	ServiceRunning bool `json:"service_running"`
	// ServiceInstalled and ServiceOutdated describe the installed service
	// definition (collector.md §4.8). The CLI fills them in.
	ServiceInstalled bool `json:"service_installed"`
	ServiceOutdated  bool `json:"service_outdated"`

	Hub     Hub      `json:"hub"`
	Pending *int     `json:"pending_uploads"`
	Sources []Source `json:"sources"`
}

// Hub is the Collector's view of its Hub connection.
type Hub struct {
	// Reachable is nil until the Hub has been contacted.
	Reachable *bool      `json:"reachable"`
	LastSync  *time.Time `json:"last_sync"`
	LastError *Failure   `json:"last_error"`
}

// Source is one Source's line in the report.
type Source struct {
	ID       string   `json:"id"`
	Root     string   `json:"root"`
	Enabled  bool     `json:"enabled"`
	Detected bool     `json:"detected"`
	Layouts  []string `json:"layouts"`
	// Records counts every discovered Raw record, excluded ones included.
	Records   int      `json:"records"`
	Excluded  *int     `json:"excluded"`
	LastError *Failure `json:"last_error"`
}

// Failure is an error and when it happened.
type Failure struct {
	Message string    `json:"message"`
	At      time.Time `json:"at"`
}

// Format writes r as the text report, with times relative to now.
func Format(w io.Writer, r Report, now time.Time) {
	service := "not running"
	if r.ServiceRunning {
		service = "running"
	}
	fmt.Fprintf(w, "agent-history %s   machine %s  %q   service: %s\n", r.Version, shortID(r.MachineID), r.DisplayName, service)

	hub := "not contacted yet"
	if r.Hub.Reachable != nil {
		hub = "unreachable"
		if *r.Hub.Reachable {
			hub = "reachable"
		}
	}
	if r.Hub.LastSync != nil {
		hub += ", last sync " + ago(*r.Hub.LastSync, now)
	}
	fmt.Fprintf(w, "Hub      %s  %s\n", r.HubURL, hub)
	if e := r.Hub.LastError; e != nil {
		fmt.Fprintf(w, "         last error %s: %s\n", ago(e.At, now), e.Message)
	}

	if r.Pending != nil {
		fmt.Fprintf(w, "Uploads  %d pending\n", *r.Pending)
	}
	switch {
	case !r.ServiceInstalled:
		fmt.Fprintln(w, "Service  not installed, run `agent-history service install`")
	case r.ServiceOutdated:
		fmt.Fprintln(w, "Service  service definition outdated, run `agent-history service install`")
	}

	fmt.Fprintln(w)
	for _, s := range r.Sources {
		if !s.Enabled {
			fmt.Fprintf(w, "%-12s disabled\n", s.ID)
			continue
		}
		state := "not detected"
		if s.Detected {
			state = "detected"
		}
		fmt.Fprintf(w, "%-12s %-14s %s\n", s.ID, state, s.Root)
		if s.Detected {
			records := commas(s.Records)
			if s.Excluded != nil && *s.Excluded > 0 {
				records += fmt.Sprintf(" (%s excluded)", commas(*s.Excluded))
			}
			fmt.Fprintf(w, "%-12s layouts: %s   records: %s\n", "", strings.Join(s.Layouts, " + "), records)
		}
		if e := s.LastError; e != nil {
			fmt.Fprintf(w, "%-12s last error %s: %s\n", "", ago(e.At, now), e.Message)
		}
	}
}

// shortID is the first 8 characters of a Machine id.
func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8] + "…"
}

// ago is t relative to now, e.g. "12s ago" or "3h ago".
func ago(t, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d/time.Hour))
	default:
		return "on " + t.Local().Format("2006-01-02 15:04")
	}
}

// commas formats n with thousands separators.
func commas(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0 && s[i-1] != '-'; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
