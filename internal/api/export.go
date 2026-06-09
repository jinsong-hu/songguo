package api

import (
	"encoding/csv"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/songguo/songguo/internal/calls"
	"github.com/songguo/songguo/internal/store"
)

// csvHeader is the fixed column order for CSV exports.
var csvHeader = []string{
	"ts", "token_id", "model", "modality", "vendor", "credential_id",
	"attempt", "status", "cost", "latency_ms", "stream", "err",
}

// handleCallsExport streams a downloadable export of the filtered calls as
// either CSV (default) or JSON. Rows are capped at exportMaxRows.
func (a *api) handleCallsExport(w http.ResponseWriter, r *http.Request) {
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "csv"
	}
	if format != "csv" && format != "json" {
		writeError(w, http.StatusBadRequest, "bad_request", "format must be csv or json")
		return
	}

	// Reuse the same filters as /api/calls but ignore pagination; pull up to
	// exportMaxRows in batches bounded by the store's max page size.
	f := callFilterFromQuery(r, exportMaxRows, exportMaxRows)
	f.Offset = parseIntDefault(r, "offset", 0)
	if f.Offset < 0 {
		f.Offset = 0
	}

	entries, err := a.collectForExport(f)
	if err != nil {
		a.serverError(w, "export calls", err)
		return
	}

	switch format {
	case "json":
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", `attachment; filename="calls.json"`)
		w.WriteHeader(http.StatusOK)
		views := make([]entryView, 0, len(entries))
		for _, e := range entries {
			views = append(views, newEntryView(e))
		}
		_ = json.NewEncoder(w).Encode(views)
	default: // csv
		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("Content-Disposition", `attachment; filename="calls.csv"`)
		w.WriteHeader(http.StatusOK)
		cw := csv.NewWriter(w)
		_ = cw.Write(csvHeader)
		for _, e := range entries {
			_ = cw.Write([]string{
				e.TS.UTC().Format(time.RFC3339),
				e.TokenID,
				e.Model,
				string(e.Modality),
				e.Vendor,
				e.CredentialID,
				strconv.Itoa(e.Attempt),
				strconv.Itoa(e.Status),
				strconv.FormatFloat(e.Cost, 'f', -1, 64),
				strconv.FormatInt(e.LatencyMS, 10),
				strconv.FormatBool(e.Stream),
				e.Err,
			})
		}
		cw.Flush()
	}
}

// collectForExport pages through QueryCalls (which caps a single call at its
// own maxCallsLimit) until exportMaxRows rows are gathered or the data runs
// out.
func (a *api) collectForExport(f store.CallFilter) ([]calls.Entry, error) {
	const pageSize = 1000 // store caps a single QueryCalls at 1000.
	var out []calls.Entry
	offset := f.Offset
	for len(out) < exportMaxRows {
		page := f
		page.Limit = pageSize
		page.Offset = offset
		rows, err := a.store.QueryCalls(page)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			break
		}
		for _, e := range rows {
			out = append(out, e)
			if len(out) >= exportMaxRows {
				break
			}
		}
		if len(rows) < pageSize {
			break
		}
		offset += pageSize
	}
	return out, nil
}
