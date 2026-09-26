package hubclient

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"testing"
)

func TestTransient(t *testing.T) {
	connRefused := &url.Error{Op: "Post", URL: "http://hub", Err: errors.New("connection refused")}
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{connRefused, true},
		{fmt.Errorf("wrapped: %w", connRefused), true},
		{&StatusError{StatusCode: 500}, true},
		{&StatusError{StatusCode: 503}, true},
		{&StatusError{StatusCode: 400}, false},
		{&StatusError{StatusCode: 426}, false},
		{&ConflictError{}, false},
		{&url.Error{Op: "Post", URL: "http://hub", Err: context.Canceled}, false},
		{errors.New("open: permission denied"), false},
	}
	for _, tc := range cases {
		if got := Transient(tc.err); got != tc.want {
			t.Errorf("Transient(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}
