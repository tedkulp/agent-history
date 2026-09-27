package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tedkulp/agent-history/internal/hub/store"
	"github.com/tedkulp/agent-history/protocol"
)

func TestStoreFailureStatus(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{fmt.Errorf("%w: SQLITE_BUSY", store.ErrBusy), http.StatusServiceUnavailable, protocol.ErrBusy},
		{fmt.Errorf("%w: SQLITE_FULL", store.ErrDiskFull), http.StatusInsufficientStorage, protocol.ErrInsufficientStorage},
		{errors.New("something else"), http.StatusInternalServerError, protocol.ErrInternal},
	}
	s := &server{log: slog.New(slog.DiscardHandler)}
	for _, c := range cases {
		w := httptest.NewRecorder()
		s.storeFailure(w, httptest.NewRequest(http.MethodPost, "/api/v1/machines/m/records", nil), c.err)
		var body protocol.Error
		json.NewDecoder(w.Body).Decode(&body)
		if w.Code != c.status || body.Error != c.code {
			t.Errorf("%v: %d %q, want %d %q", c.err, w.Code, body.Error, c.status, c.code)
		}
	}
}
