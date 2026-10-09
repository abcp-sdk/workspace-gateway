package workspacesvc

import (
	"testing"

	"connectrpc.com/connect/v2"
)

func nhdr(session string) *connect.Header {
	h := &connect.Header{}
	if session != "" {
		h.Set("X-Session-Name", session)
	}
	return h
}

func TestCanWriteNamespace(t *testing.T) {
	cases := []struct {
		name, resourceNS, session string
		want                      bool
	}{
		{"same", "acme", "acme:web:main", true},
		{"different", "globex", "acme:web:main", false},
		{"webui tenant-wide", "globex", "", true},
		{"legacy row tenant-wide", "", "acme:web:main", true},
		{"free session tenant-wide", "globex", "freesession", true},
	}
	for _, c := range cases {
		if got := canWrite(c.resourceNS, nhdr(c.session)); got != c.want {
			t.Fatalf("%s: canWrite(%q,%q)=%v want %v", c.name, c.resourceNS, c.session, got, c.want)
		}
	}
}

func TestCallerNamespace(t *testing.T) {
	if got := callerNamespace(nhdr("acme:web:main")); got != "acme" {
		t.Fatalf("callerNamespace=%q", got)
	}
	if got := callerNamespace(nhdr("")); got != "" {
		t.Fatalf("webui callerNamespace=%q", got)
	}
}
