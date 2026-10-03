package cli

import (
	"runtime/debug"
	"strings"
	"testing"
)

func TestSourceVersionUsesBuildMetadata(t *testing.T) {
	for _, scenario := range []struct {
		info *debug.BuildInfo
		want string
	}{
		{nil, "dev"},
		{&debug.BuildInfo{}, "dev"},
		{&debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "vcs", Value: "git"}, {Key: "vcs.revision", Value: ""}}}, "dev"},
		{&debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: strings.Repeat("a", 40)}}}, "sha-" + strings.Repeat("a", 40)},
	} {
		if got := sourceVersion(scenario.info); got != scenario.want {
			t.Fatal(got, scenario.want)
		}
	}
}

func TestBinaryVersionUsesInjectedVersionAndFallsBackToBuildInfo(t *testing.T) {
	old := version
	t.Cleanup(func() { version = old })
	for _, v := range []string{"v1.2.3", "sha-" + strings.Repeat("a", 40)} {
		version = v
		if got := binaryVersion(); got != v {
			t.Fatal(got, v)
		}
	}
	info, _ := debug.ReadBuildInfo()
	for _, v := range []string{"", "dev"} {
		version = v
		if got := binaryVersion(); got != sourceVersion(info) {
			t.Fatal(got)
		}
	}
}
