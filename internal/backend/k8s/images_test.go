package k8s

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

var digest = "@sha256:" + strings.Repeat("ab", 32)

func TestImageTag(t *testing.T) {
	for in, want := range map[string]string{
		"v1.2.3":      "1.2.3",
		"1.2.3":       "1.2.3",
		"v1.2.3-rc.1": "1.2.3-rc.1",
		"dev":         "main",
		"main":        "main",
		"12/merge":    "main",
		"v1.2":        "main",
		"":            "main",
	} {
		if got := ImageTag(in); got != want {
			t.Errorf("ImageTag(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseOverrides(t *testing.T) {
	got, err := ParseOverrides(" device=reg/dev@sha256:1 ,pagewire=reg/ps:1, driver-hplip=reg/h:2 ")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"device": "reg/dev@sha256:1", "pagewire": "reg/ps:1", "driver-hplip": "reg/h:2"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if m, err := ParseOverrides(""); err != nil || len(m) != 0 {
		t.Errorf("empty: %v, %v", m, err)
	}
	for _, bad := range []string{"device", "device=", "=x", "nosuch=reg/x:1", "device=a,device=b"} {
		if _, err := ParseOverrides(bad); err == nil {
			t.Errorf("ParseOverrides(%q): want error", bad)
		}
	}
}

func TestImageRef(t *testing.T) {
	if got := ImageRef("ghcr.io/thephaseless", "1.2.3", "pagewire-device", nil); got != "ghcr.io/thephaseless/pagewire-device:1.2.3" {
		t.Errorf("default: %s", got)
	}
	ov := map[string]string{"driver-ps": "localhost:5000/ps:x", "pagewire": "localhost:5000/self:y"}
	if got := ImageRef("ghcr.io/thephaseless", "1.2.3", "pagewire-driver-ps", ov); got != "localhost:5000/ps:x" {
		t.Errorf("override: %s", got)
	}
	if got := ImageRef("ghcr.io/thephaseless", "1.2.3", "pagewire", ov); got != "localhost:5000/self:y" {
		t.Errorf("self override: %s", got)
	}
	if got := ImageRef("ghcr.io/thephaseless", "1.2.3", "pagewire-driver-hplip", ov); got != "ghcr.io/thephaseless/pagewire-driver-hplip:1.2.3" {
		t.Errorf("other key: %s", got)
	}
}

func TestPullPolicy(t *testing.T) {
	for ref, want := range map[string]corev1.PullPolicy{
		"ghcr.io/thephaseless/pagewire-device:main":          corev1.PullAlways,
		"ghcr.io/thephaseless/pagewire-device:1.2.3":         corev1.PullIfNotPresent,
		"ghcr.io/thephaseless/pagewire-device:main" + digest: corev1.PullIfNotPresent,
		"ghcr.io/thephaseless/pagewire-device" + digest:      corev1.PullIfNotPresent,
		"localhost:5000/x:0.0.0-test":                        corev1.PullIfNotPresent,
		"localhost:5000/x:main":                              corev1.PullAlways,
	} {
		if got := PullPolicy(ref); got != want {
			t.Errorf("PullPolicy(%q) = %s, want %s", ref, got, want)
		}
	}
}
