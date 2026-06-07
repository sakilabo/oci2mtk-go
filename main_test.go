package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func gz(data []byte) []byte {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	w.Write(data)
	w.Close()
	return buf.Bytes()
}

// writeTar writes files to disk as a TAR; gzip-compressed if name ends with .gz
func writeTar(t *testing.T, file string, files map[string][]byte) {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, data := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write(data)
	}
	tw.Close()
	out := buf.Bytes()
	if filepath.Ext(file) == ".gz" {
		out = gz(out)
	}
	if err := os.WriteFile(file, out, 0o644); err != nil {
		t.Fatal(err)
	}
}

// readTar reads a TAR / tar.gz file into an entry-name to content map (for verification)
func readTar(t *testing.T, file string) map[string][]byte {
	t.Helper()
	f, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var r io.Reader = f
	magic := make([]byte, 2)
	n, _ := io.ReadFull(f, magic)
	f.Seek(0, io.SeekStart)
	if n == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		zr, err := gzip.NewReader(f)
		if err != nil {
			t.Fatal(err)
		}
		defer zr.Close()
		r = zr
	}
	m := make(map[string][]byte)
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(tr)
		m[hdr.Name] = data
	}
	return m
}

// ociFiles builds the files of a single-manifest OCI image; the layer is gzip
func ociFiles(manifests []ociDescriptor) map[string][]byte {
	idx := ociIndex{Manifests: manifests}
	idxData, _ := json.Marshal(idx)
	return map[string][]byte{
		"oci-layout":       []byte(`{"imageLayoutVersion":"1.0.0"}`),
		"index.json":       idxData,
		"blobs/sha256/cfg": []byte(`{"architecture":"amd64","os":"linux"}`),
		"blobs/sha256/lay": gz([]byte("LAYER-CONTENT")),
	}
}

func ociManifestBlob() []byte {
	m := ociManifest{
		Config: ociDescriptor{MediaType: "application/vnd.oci.image.config.v1+json", Digest: "sha256:cfg"},
		Layers: []ociDescriptor{{MediaType: "application/vnd.oci.image.layer.v1.tar+gzip", Digest: "sha256:lay"}},
	}
	data, _ := json.Marshal(m)
	return data
}

func runProcess(t *testing.T, files map[string][]byte, o options) (int, map[string][]byte) {
	t.Helper()
	dir := t.TempDir()
	in := filepath.Join(dir, "in.tar")
	writeTar(t, in, files)
	o.inFile = in
	if o.outFile == "" && !o.dryRun {
		o.outFile = filepath.Join(dir, "out.tar")
	}
	code := process(o)
	var out map[string][]byte
	if o.outFile != "" {
		if _, err := os.Stat(o.outFile); err == nil {
			out = readTar(t, o.outFile)
		}
	}
	return code, out
}

func TestConvertOCISingle(t *testing.T) {
	files := ociFiles([]ociDescriptor{
		{MediaType: "application/vnd.oci.image.manifest.v1+json", Digest: "sha256:man"},
	})
	files["blobs/sha256/man"] = ociManifestBlob()

	code, out := runProcess(t, files, options{})
	if code != exitOK {
		t.Fatalf("code = %d, want %d", code, exitOK)
	}
	if string(out["config.json"]) == "" {
		t.Error("config.json missing")
	}
	if string(out["layer0.tar"]) != "LAYER-CONTENT" {
		t.Errorf("layer0.tar = %q, want decompressed content", out["layer0.tar"])
	}
	var man []dockerManifestEntry
	if err := json.Unmarshal(out["manifest.json"], &man); err != nil {
		t.Fatal(err)
	}
	if man[0].Config != "config.json" || len(man[0].Layers) != 1 || man[0].Layers[0] != "layer0.tar" {
		t.Errorf("manifest = %+v", man[0])
	}
}

func TestConvertOCIMultiArch(t *testing.T) {
	mans := []ociDescriptor{
		{MediaType: "application/vnd.oci.image.manifest.v1+json", Digest: "sha256:a", Platform: &ociPlatform{OS: "linux", Architecture: "amd64"}},
		{MediaType: "application/vnd.oci.image.manifest.v1+json", Digest: "sha256:b", Platform: &ociPlatform{OS: "linux", Architecture: "arm64"}},
	}
	files := ociFiles(mans)
	files["blobs/sha256/a"] = ociManifestBlob()
	files["blobs/sha256/b"] = ociManifestBlob()

	if code, _ := runProcess(t, files, options{dryRun: true}); code != exitFail {
		t.Errorf("no platform: code = %d, want %d", code, exitFail)
	}
	if code, out := runProcess(t, files, options{platform: "linux/arm64"}); code != exitOK {
		t.Errorf("with platform: code = %d, want %d", code, exitOK)
	} else if string(out["layer0.tar"]) != "LAYER-CONTENT" {
		t.Errorf("layer not converted: %q", out["layer0.tar"])
	}
	if code, _ := runProcess(t, files, options{platform: "windows/amd64", dryRun: true}); code != exitFail {
		t.Errorf("no match: code = %d, want %d", code, exitFail)
	}
}

func TestConvertOCINestedIndexUnsupported(t *testing.T) {
	files := ociFiles([]ociDescriptor{
		{MediaType: "application/vnd.oci.image.index.v1+json", Digest: "sha256:nested"},
	})
	if code, _ := runProcess(t, files, options{dryRun: true}); code != exitFail {
		t.Errorf("nested index: code = %d, want %d", code, exitFail)
	}
}

func TestConvertOCIZstdFails(t *testing.T) {
	files := ociFiles([]ociDescriptor{
		{MediaType: "application/vnd.oci.image.manifest.v1+json", Digest: "sha256:man"},
	})
	m := ociManifest{
		Config: ociDescriptor{Digest: "sha256:cfg"},
		Layers: []ociDescriptor{{MediaType: "application/vnd.oci.image.layer.v1.tar+zstd", Digest: "sha256:lay"}},
	}
	manData, _ := json.Marshal(m)
	files["blobs/sha256/man"] = manData
	files["blobs/sha256/lay"] = append([]byte{0x28, 0xb5, 0x2f, 0xfd}, []byte("x")...)

	if code, _ := runProcess(t, files, options{dryRun: true}); code != exitFail {
		t.Errorf("zstd: code = %d, want %d", code, exitFail)
	}
}

func TestConvertDockerNeedsConversion(t *testing.T) {
	man := []dockerManifestEntry{{
		Config:   "config_hash.json",
		RepoTags: []string{"repo:tag"},
		Layers:   []string{"abc123/layer.tar"}, // inside a directory -> needs conversion
	}}
	manData, _ := json.Marshal(man)
	files := map[string][]byte{
		"manifest.json":    manData,
		"config_hash.json": []byte(`{"os":"linux"}`),
		"abc123/layer.tar": []byte("PLAINTAR"),
	}
	code, out := runProcess(t, files, options{})
	if code != exitOK {
		t.Fatalf("code = %d, want %d", code, exitOK)
	}
	if string(out["layer0.tar"]) != "PLAINTAR" {
		t.Errorf("layer0.tar = %q", out["layer0.tar"])
	}
	var nm []dockerManifestEntry
	json.Unmarshal(out["manifest.json"], &nm)
	if len(nm[0].RepoTags) != 1 || nm[0].RepoTags[0] != "repo:tag" {
		t.Errorf("RepoTags not carried: %+v", nm[0].RepoTags)
	}
}

func TestConvertDockerMultiTag(t *testing.T) {
	man := []dockerManifestEntry{{
		Config:   "config.json",
		RepoTags: []string{"app:v1", "app:latest"}, // multiple tags
		Layers:   []string{"abc/layer.tar"},
	}}
	manData, _ := json.Marshal(man)
	files := map[string][]byte{
		"manifest.json": manData,
		"config.json":   []byte(`{}`),
		"abc/layer.tar": []byte("X"),
	}

	// no tag specified -> succeeds with an empty tag
	if code, out := runProcess(t, files, options{}); code != exitOK {
		t.Errorf("no tag: code = %d, want %d", code, exitOK)
	} else {
		var nm []dockerManifestEntry
		json.Unmarshal(out["manifest.json"], &nm)
		if len(nm[0].RepoTags) != 0 {
			t.Errorf("no tag: RepoTags = %+v, want []", nm[0].RepoTags)
		}
	}
	// non-matching tag -> error
	if code, _ := runProcess(t, files, options{tag: "app:none", dryRun: true}); code != exitFail {
		t.Errorf("bad tag: code = %d, want %d", code, exitFail)
	}
	// tag specified -> only that single tag
	code, out := runProcess(t, files, options{tag: "app:latest"})
	if code != exitOK {
		t.Fatalf("with tag: code = %d, want %d", code, exitOK)
	}
	var nm []dockerManifestEntry
	json.Unmarshal(out["manifest.json"], &nm)
	if len(nm[0].RepoTags) != 1 || nm[0].RepoTags[0] != "app:latest" {
		t.Errorf("RepoTags = %+v, want [app:latest]", nm[0].RepoTags)
	}
}

func TestConvertDockerMultiImage(t *testing.T) {
	// a manifest.json containing multiple images, as with docker save img1 img2
	man := []dockerManifestEntry{
		{Config: "c1.json", RepoTags: []string{"img1:latest"}, Layers: []string{"a/layer.tar"}},
		{Config: "c2.json", RepoTags: []string{"img2:latest"}, Layers: []string{"b/layer.tar"}},
	}
	manData, _ := json.Marshal(man)
	files := map[string][]byte{
		"manifest.json": manData,
		"c1.json":       []byte(`{"n":1}`),
		"c2.json":       []byte(`{"n":2}`),
		"a/layer.tar":   []byte("LAYER-A"),
		"b/layer.tar":   []byte("LAYER-B"),
	}

	// not specified -> error since selection is required for multiple images
	if code, _ := runProcess(t, files, options{dryRun: true}); code != exitFail {
		t.Errorf("no tag: code = %d, want %d", code, exitFail)
	}
	// select img2 -> uses that image's config / layer
	code, out := runProcess(t, files, options{tag: "img2:latest"})
	if code != exitOK {
		t.Fatalf("with tag: code = %d, want %d", code, exitOK)
	}
	if string(out["config.json"]) != `{"n":2}` {
		t.Errorf("config.json = %q, want img2 config", out["config.json"])
	}
	if string(out["layer0.tar"]) != "LAYER-B" {
		t.Errorf("layer0.tar = %q, want LAYER-B", out["layer0.tar"])
	}
	var nm []dockerManifestEntry
	json.Unmarshal(out["manifest.json"], &nm)
	if len(nm[0].RepoTags) != 1 || nm[0].RepoTags[0] != "img2:latest" {
		t.Errorf("RepoTags = %+v, want [img2:latest]", nm[0].RepoTags)
	}
}

func TestConvertDockerNotNeeded(t *testing.T) {
	man := []dockerManifestEntry{{
		Config: "config.json",
		Layers: []string{"layer0.tar"}, // top-level .tar -> no conversion needed
	}}
	manData, _ := json.Marshal(man)
	files := map[string][]byte{
		"manifest.json": manData,
		"config.json":   []byte(`{}`),
		"layer0.tar":    []byte("X"),
	}
	if code, _ := runProcess(t, files, options{dryRun: true}); code != exitNotNeeded {
		t.Errorf("code = %d, want %d", code, exitNotNeeded)
	}
}

func TestConvertDockerGzipLayerByMagic(t *testing.T) {
	man := []dockerManifestEntry{{
		Config: "config.json",
		Layers: []string{"blobs/sha256/xyz"}, // inside a directory -> needs conversion
	}}
	manData, _ := json.Marshal(man)
	files := map[string][]byte{
		"manifest.json":    manData,
		"config.json":      []byte(`{}`),
		"blobs/sha256/xyz": gz([]byte("DECOMPRESSED")),
	}
	code, out := runProcess(t, files, options{})
	if code != exitOK {
		t.Fatalf("code = %d", code)
	}
	if string(out["layer0.tar"]) != "DECOMPRESSED" {
		t.Errorf("gzip layer not decompressed by magic")
	}
}

func TestGzipInputAndOutput(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.tar.gz") // gzip-compressed input
	files := ociFiles([]ociDescriptor{
		{MediaType: "application/vnd.oci.image.manifest.v1+json", Digest: "sha256:man"},
	})
	files["blobs/sha256/man"] = ociManifestBlob()
	writeTar(t, in, files)

	out := filepath.Join(dir, "out.tar.gz") // gzip-compressed output
	if code := process(options{inFile: in, outFile: out}); code != exitOK {
		t.Fatalf("code = %d", code)
	}
	back := readTar(t, out)
	if string(back["layer0.tar"]) != "LAYER-CONTENT" {
		t.Errorf("round trip layer = %q", back["layer0.tar"])
	}
	if _, ok := back["manifest.json"]; !ok {
		t.Error("manifest.json missing")
	}
}

func TestOverwriteGuard(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.tar")
	files := ociFiles([]ociDescriptor{
		{MediaType: "application/vnd.oci.image.manifest.v1+json", Digest: "sha256:man"},
	})
	files["blobs/sha256/man"] = ociManifestBlob()
	writeTar(t, in, files)
	out := filepath.Join(dir, "out.tar")
	os.WriteFile(out, []byte("existing"), 0o644)

	if code := process(options{inFile: in, outFile: out}); code != exitFail {
		t.Errorf("no -f: code = %d, want %d", code, exitFail)
	}
	if code := process(options{inFile: in, outFile: out, force: true}); code != exitOK {
		t.Errorf("with -f: code = %d, want %d", code, exitOK)
	}
}

func TestNoTempFilesLeftBehind(t *testing.T) {
	files := ociFiles([]ociDescriptor{
		{MediaType: "application/vnd.oci.image.manifest.v1+json", Digest: "sha256:man"},
	})
	files["blobs/sha256/man"] = ociManifestBlob()
	if code, _ := runProcess(t, files, options{}); code != exitOK {
		t.Fatalf("code = %d", code)
	}
	matches, _ := filepath.Glob(filepath.Join(os.TempDir(), "oci2mtk-*.tar"))
	if len(matches) != 0 {
		t.Errorf("temporary files left behind: %v", matches)
	}
}
