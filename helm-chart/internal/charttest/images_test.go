package charttest

import "testing"

func TestRewriteImage(t *testing.T) {
	for _, tc := range []struct{ ref, want string }{
		{"public.ecr.aws/devzeroinc/zxporter:v1", "m.example/p/devzeroinc/zxporter:v1"},
		{"devzeroinc/zxporter-netmon:v1", "m.example/p/devzeroinc/zxporter-netmon:v1"},
		{"docker.io/devzeroinc/criu:4.2.1", "m.example/p/devzeroinc/criu:4.2.1"},
		{"alpine:3.19", "m.example/p/library/alpine:3.19"},
		{"docker.io/alpine", "m.example/p/library/alpine"},
		{"localhost:5000/x", "m.example/p/x"},
		{"MYREGISTRY/team/app", "m.example/p/team/app"},
		{"ghcr.io/a/b@sha256:abc", "m.example/p/a/b@sha256:abc"},
		{"ghcr.io/a/b:v1@sha256:abc", "m.example/p/a/b:v1@sha256:abc"},
	} {
		if got := RewriteImage("m.example/p/", tc.ref); got != tc.want {
			t.Errorf("RewriteImage(%q) = %q, want %q", tc.ref, got, tc.want)
		}
	}
}
