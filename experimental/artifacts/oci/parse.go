package oci

import "fmt"

// Reference is a parsed OCI reference.
type Reference struct {
	Registry   string // e.g. "123456789.dkr.ecr.us-west-2.amazonaws.com"
	Repository string // e.g. "graphs/networking"
	Tag        string // e.g. "v1.0.0" (empty if digest-pinned)
	Digest     string // e.g. "sha256:abc..." (empty if tag-based)
}

// String returns the reference in its canonical form.
func (r Reference) String() string {
	base := r.Registry + "/" + r.Repository
	if r.Digest != "" {
		return base + "@" + r.Digest
	}
	return base + ":" + r.Tag
}

// ParseReference parses an OCI reference string into its components.
// Accepted forms:
//
//	registry/repo:tag
//	registry/repo/nested/path:tag
//	registry/repo@sha256:abc123
func ParseReference(ref string) (Reference, error) {
	if ref == "" {
		return Reference{}, fmt.Errorf("empty reference")
	}

	// Split registry from the rest at first '/'.
	slash := -1
	for i, c := range ref {
		if c == '/' {
			slash = i
			break
		}
	}
	if slash <= 0 {
		return Reference{}, fmt.Errorf("invalid reference %q: missing registry", ref)
	}
	registry := ref[:slash]
	rest := ref[slash+1:]
	if rest == "" {
		return Reference{}, fmt.Errorf("invalid reference %q: missing repository", ref)
	}

	// Check for digest-pinned reference (contains '@').
	if at := lastIndex(rest, '@'); at >= 0 {
		repo := rest[:at]
		digest := rest[at+1:]
		if repo == "" || digest == "" {
			return Reference{}, fmt.Errorf("invalid reference %q: empty repository or digest", ref)
		}
		return Reference{Registry: registry, Repository: repo, Digest: digest}, nil
	}

	// Check for tag-based reference (contains ':').
	if colon := lastIndex(rest, ':'); colon >= 0 {
		repo := rest[:colon]
		tag := rest[colon+1:]
		if repo == "" || tag == "" {
			return Reference{}, fmt.Errorf("invalid reference %q: empty repository or tag", ref)
		}
		return Reference{Registry: registry, Repository: repo, Tag: tag}, nil
	}

	return Reference{}, fmt.Errorf("invalid reference %q: must contain tag (:) or digest (@)", ref)
}

func lastIndex(s string, b byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == b {
			return i
		}
	}
	return -1
}
