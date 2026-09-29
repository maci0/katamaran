package dashboard

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"testing"
)

// These two files are the vendored-asset manifest itself, not vendored code,
// so they carry no digest of their own.
const (
	assetChecksumsFile = "SHA256SUMS"
	assetProvenanceDoc = "ATTRIBUTION.md"
)

// TestVendoredAssetsMatchSHA256SUMS pins the vendored third-party bundles to
// the digests recorded in assets/SHA256SUMS. A bundle edited in place, or
// swapped for a build of a different version, changes its digest and fails
// here instead of shipping to the dashboard origin unreviewed.
func TestVendoredAssetsMatchSHA256SUMS(t *testing.T) {
	want, err := readAssetChecksums()
	if err != nil {
		t.Fatalf("read %s: %v", assetChecksumsFile, err)
	}
	if len(want) == 0 {
		t.Fatalf("%s lists no files", assetChecksumsFile)
	}

	shipped, err := shippedAssetNames()
	if err != nil {
		t.Fatalf("list embedded assets: %v", err)
	}
	if len(shipped) == 0 {
		t.Fatal("no assets embedded; the vendored bundles went missing")
	}

	for _, name := range shipped {
		if name == assetChecksumsFile || name == assetProvenanceDoc {
			continue
		}
		digest, ok := want[name]
		if !ok {
			t.Errorf("%s: vendored asset has no digest in %s; record one with "+
				"sha256sum before shipping it", name, assetChecksumsFile)
			continue
		}
		got, err := assetDigest(name)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got != digest {
			t.Errorf("%s: digest %s does not match %s", name, got, digest)
		}
	}

	for name := range want {
		if !slices.Contains(shipped, name) {
			t.Errorf("%s: listed in %s but not embedded", name, assetChecksumsFile)
		}
	}
}

// readAssetChecksums parses assets/SHA256SUMS in the sha256sum(1) format:
// a hex digest, whitespace, then the file name.
func readAssetChecksums() (map[string]string, error) {
	body, err := assetsFS.ReadFile("assets/" + assetChecksumsFile)
	if err != nil {
		return nil, err
	}
	sums := make(map[string]string)
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 {
			return nil, fmt.Errorf("malformed line: %q", line)
		}
		if _, dup := sums[fields[1]]; dup {
			return nil, fmt.Errorf("duplicate entry for %q", fields[1])
		}
		sums[fields[1]] = fields[0]
	}
	return sums, nil
}

// shippedAssetNames lists the files under assets/ in the embedded FS, which is
// the set of bytes the dashboard binary actually carries.
func shippedAssetNames() ([]string, error) {
	entries, err := assetsFS.ReadDir("assets")
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	return names, nil
}

// assetDigest hashes the embedded copy, not the file on disk: the test pins
// what is served, and a built binary carries the embed, not the tree.
func assetDigest(name string) (string, error) {
	body, err := fs.ReadFile(assetsFS, "assets/"+name)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}
