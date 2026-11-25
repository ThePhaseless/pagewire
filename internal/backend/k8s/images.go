// Package k8s is the Kubernetes device backend: it renders the device pod and
// applies it with client-go server-side apply.
package k8s

import (
	"fmt"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// SelfContainer is the container of the owner Deployment whose image becomes
// Params.SelfImage (probe, agent and fetch-blobs).
const SelfContainer = "pagewire"

// DefaultTag is the image tag for any version that is not a release.
const DefaultTag = "main"

var semverRe = regexp.MustCompile(`^v?(\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?)$`)

// ImageTag maps a build version to the device and driver image tag: a semver
// with an optional leading v gives the bare semver, anything else "main".
func ImageTag(version string) string {
	if m := semverRe.FindStringSubmatch(version); m != nil {
		return m[1]
	}
	return DefaultTag
}

var overrideKeys = map[string]bool{
	"device": true, "driver-ps": true, "driver-hplip": true,
	"driver-gutenprint": true, "driver-ghostscript": true, "driver-legacy": true, "pagewire": true,
}

// ParseOverrides parses PAGEWIRE_IMAGE_OVERRIDES ("device=ref,driver-ps=ref,...").
// Unknown, repeated or malformed entries are errors.
func ParseOverrides(s string) (map[string]string, error) {
	out := map[string]string{}
	for entry := range strings.SplitSeq(s, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		k, v, ok := strings.Cut(entry, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch {
		case !ok || k == "" || v == "":
			return nil, fmt.Errorf("image override %q: want key=ref", entry)
		case !overrideKeys[k]:
			return nil, fmt.Errorf("image override %q: unknown key %q", entry, k)
		case out[k] != "":
			return nil, fmt.Errorf("image override %q: key %q repeated", entry, k)
		}
		out[k] = v
	}
	return out, nil
}

// ImageRef returns <registry>/<name>:<tag>, or the override for name. Override
// keys are the image names without the "pagewire-" prefix ("pagewire" stays).
func ImageRef(registry, tag, name string, overrides map[string]string) string {
	if ref := overrides[strings.TrimPrefix(name, "pagewire-")]; ref != "" {
		return ref
	}
	return registry + "/" + name + ":" + tag
}

// PullPolicy is Always for an undigested ref tagged main (it moves with every
// push) and IfNotPresent otherwise.
func PullPolicy(ref string) corev1.PullPolicy {
	if strings.Contains(ref, "@sha256:") {
		return corev1.PullIfNotPresent
	}
	last := ref[strings.LastIndex(ref, "/")+1:]
	if _, tag, ok := strings.Cut(last, ":"); ok && tag == DefaultTag {
		return corev1.PullAlways
	}
	return corev1.PullIfNotPresent
}
