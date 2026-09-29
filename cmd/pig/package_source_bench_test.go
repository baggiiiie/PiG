package main

import (
	"strings"
	"testing"
)

func BenchmarkPackageSourceBoundaries(b *testing.B) {
	for _, tc := range []struct{ name, source string }{
		{"empty", ""},
		{"bare-name", "demo"},
		{"bare-scp", "git@github.com:user/repo"},
		{"npm", "npm:@scope/pkg@1.2.3"},
		{"git", "git:github.com/user/repo@main"},
		{"long-local", strings.Repeat("nested/", 1024) + "package"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			base := b.TempDir()
			b.ReportAllocs()
			for b.Loop() {
				_ = detectSourceKind(tc.source)
				_ = packageSourceIdentity(base, tc.source)
			}
		})
	}
}
