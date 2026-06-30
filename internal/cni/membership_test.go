package cni

import (
	"testing"
)

func TestParseCNIArgs(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want CNIArgs
	}{
		{
			name: "kubernetes args",
			raw:  "K8S_POD_NAME=foo,K8S_POD_NAMESPACE=default,K8S_POD_UID=abc-123,IgnoreUnknown=1",
			want: CNIArgs{
				"K8S_POD_NAME":      "foo",
				"K8S_POD_NAMESPACE": "default",
				"K8S_POD_UID":       "abc-123",
				"IgnoreUnknown":     "1",
			},
		},
		{
			name: "empty",
			raw:  "",
			want: CNIArgs{},
		},
		{
			name: "whitespace trimmed",
			raw:  " K8S_POD_NAME = bar , K8S_POD_NAMESPACE = ns ",
			want: CNIArgs{
				"K8S_POD_NAME":      "bar",
				"K8S_POD_NAMESPACE": "ns",
			},
		},
		{
			name: "value with equals (not a delimiter)",
			raw:  "K8S_POD_NAME=baz,K8S_POD_NAMESPACE=ns",
			want: CNIArgs{
				"K8S_POD_NAME":      "baz",
				"K8S_POD_NAMESPACE": "ns",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseCNIArgs(tt.raw)
			if len(got) != len(tt.want) {
				t.Fatalf("got %d keys, want %d (%+v)", len(got), len(tt.want), got)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("key %q: got %q, want %q", k, got[k], v)
				}
			}
		})
	}
}
