// oci2mtk: a command that converts container image files for MikroTik RouterOS
// - supports .tar / .tar.gz files created by `docker save`
// - supports OCI and docker-archive container images
package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

// exit codes
const (
	exitOK        = 0 // conversion succeeded
	exitNotNeeded = 1 // conversion not needed
	exitFail      = 2 // conversion failed
	exitUsage     = 3 // argument error
)

// upper limit on the number of layers (1,000 = 0 to 999)
const maxLayers = 1000

// number of leading bytes kept for every file (used to detect the compression format)
const headLen = 8

// maximum size of a file whose content is read during the archive scan
const maxIndexedSize = 16 * 1024

func main() {
	os.Exit(run(os.Args))
}

type options struct {
	inFile   string
	outFile  string
	platform string
	tag      string
	dryRun   bool
	force    bool
}

func run(argv []string) int {
	if len(argv) < 2 {
		usage()
		return exitUsage
	}

	inFile := argv[1]
	if strings.HasPrefix(inFile, "-") {
		// the first argument is fixed as the input file name; a flag here is a misuse
		usage()
		return exitUsage
	}

	var o options
	o.inFile = inFile
	fs := flag.NewFlagSet("oci2mtk", flag.ContinueOnError)
	fs.BoolVar(&o.dryRun, "d", false, "dry run")
	fs.BoolVar(&o.force, "f", false, "overwrite OUT_FILE")
	fs.StringVar(&o.platform, "p", "", "target platform os/arch[/variant]")
	fs.StringVar(&o.tag, "t", "", "target tag (RepoTag)")
	fs.StringVar(&o.outFile, "o", "", "output file")
	if err := fs.Parse(argv[2:]); err != nil {
		return exitUsage
	}

	if !o.dryRun && o.outFile == "" {
		fmt.Fprintln(os.Stderr, "error: output file (-o) is required (except in a dry run).")
		return exitUsage
	}

	return process(o)
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: oci2mtk <IN_FILE> [-d] [-f] [-p <PLATFORM>] [-t <TAG>] [-o <OUT_FILE>]")
}

// information about the image structure carried through the conversion
type parsedDocs struct {
	ociIndex    *ociIndex    // index.json
	ociManName  string       // entry name of the manifest blob written in index.json
	ociTag      string       // ref name of the selected manifest (output tag, empty if none)
	ociManErr   error        // manifest error (selection failure, unsupported, invalid digest)
	ociManifest *ociManifest // parse result of the selected manifest

	// the single manifest narrowed down when reading manifest.json (RepoTags also reduced to at most one)
	dockerManifest *dockerManifestEntry
	dockerErr      error // parse/selection error of manifest.json
}

func process(o options) int {
	var docs parsedDocs

	// scan the input image archive and build the index
	index, err := buildIndex(o.inFile, o.platform, o.tag, &docs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to read input: %v.\n", err)
		return exitFail
	}

	plan, code := buildPlan(o.inFile, index, &docs)
	if code != exitOK {
		return code
	}

	if !o.force {
		if _, statErr := os.Stat(o.outFile); statErr == nil {
			fmt.Fprintf(os.Stderr, "error: output file already exists: %s.\n"+
				"add the -f flag to overwrite.\n", o.outFile)
			return exitFail
		}
	}

	if o.dryRun {
		fmt.Println("dry run: conversion is possible. no data will be written to disk.")
		return exitOK
	}

	if err := writeOutput(o.outFile, o.inFile, plan); err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to write output: %v.\n", err)
		return exitFail
	}

	fmt.Printf("converted: %s.\n", o.outFile)
	return exitOK
}

// --- index ---

// index information for an entry in the input archive
// the leading bytes and the whole content are kept separately
type entryInfo struct {
	path string // entry path
	size int64  // entry size
	head []byte // leading 8 bytes (used to detect the file type)
	data []byte // whole content (only for some entries, nil otherwise)
}

// scan the archive and build the index
//   - keep the size and leading bytes of each entry
//   - read the content of index.json and the manifest
//     however, the OCI manifest may not be read depending on its size or read order
func buildIndex(file, platform, tag string, docs *parsedDocs) (map[string]*entryInfo, error) {
	index := make(map[string]*entryInfo)
	err := eachEntry(file, func(name string, hdr *tar.Header, tr *tar.Reader) error {
		// ignore entries with an empty name
		if name == "" {
			return nil
		}
		// read the leading bytes
		head := make([]byte, headLen)
		n, err := io.ReadFull(tr, head)
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			return err
		}
		head = head[:n]
		info := &entryInfo{path: name, head: head, size: hdr.Size}
		index[name] = info

		// check the conditions for reading the file content
		topMeta := isTopLevel(name) && (name == "index.json" || name == "manifest.json" || name == "oci-layout")
		ociMan := docs.ociIndex != nil && name == docs.ociManName // the manifest designated by the OCI index
		small := hdr.Size <= maxIndexedSize                       // small files (e.g. JSON whose purpose is not yet known)
		// stop here for files whose content is not read
		if !topMeta && !ociMan && !small {
			return nil
		}

		// read the file content
		rest, err := io.ReadAll(tr)
		if err != nil {
			return err
		}
		content := append(append([]byte{}, head...), rest...)
		info.data = content

		// parse the content of the index and manifest and record it in docs
		switch {
		case topMeta && name == "index.json":
			var idx ociIndex
			if err := json.Unmarshal(content, &idx); err == nil {
				docs.ociIndex = &idx
				// select and finalize the target manifest here. once the name is known,
				// the manifest body can be read from a later entry
				resolveManifest(&idx, platform, tag, docs)
			}
		case topMeta && name == "manifest.json":
			// narrow down to a single manifest and a single tag
			resolveDocker(content, tag, docs)
		case ociMan:
			var man ociManifest
			if err := json.Unmarshal(content, &man); err == nil {
				docs.ociManifest = &man
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(index) == 0 {
		return nil, errors.New("could not read the archive")
	}
	return index, nil
}

// sentinel error used to stop eachEntry early
var errStop = errors.New("stop")

// open a TAR / tar.gz and call fn for each regular file entry
// the scan stops when fn returns errStop
func eachEntry(file string, fn func(name string, hdr *tar.Header, tr *tar.Reader) error) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()

	var r io.Reader = f
	// detect gzip by its magic bytes rather than the file extension
	magic := make([]byte, 2)
	n, _ := io.ReadFull(f, magic)
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if n == 2 && magic[0] == magicGzip[0] && magic[1] == magicGzip[1] {
		zr, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer zr.Close()
		r = zr
	}

	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		// tar.Reader normalizes the legacy TypeRegA to TypeReg / TypeDir on read,
		// so only TypeReg needs to be checked here
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if err := fn(normName(hdr.Name), hdr, tr); err != nil {
			if err == errStop {
				return nil
			}
			return err
		}
	}
}

// return the content of an entry. use it if already kept in the index,
// otherwise scan the input to read it
func entryContent(index map[string]*entryInfo, file, name string) ([]byte, bool, error) {
	if info, ok := index[name]; ok && info.data != nil {
		return info.data, true, nil
	}
	return readEntryContent(file, name)
}

// scan the input and read the content of the specified entry (used as a fallback)
func readEntryContent(file, target string) ([]byte, bool, error) {
	var content []byte
	found := false
	err := eachEntry(file, func(name string, hdr *tar.Header, tr *tar.Reader) error {
		if name != target {
			return nil
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			return err
		}
		content = b
		found = true
		return errStop
	})
	return content, found, err
}

// --- plan ---

// output plan
type plan struct {
	repoTag string         // tag carried over to the output manifest.json
	config  plannedEntry   // config entry
	layers  []plannedEntry // layers (in output order)
}

type plannedEntry struct {
	info   *entryInfo  // input entry information
	format entryFormat // file format (currently only tar / gzip are supported)
	name   string      // name of the output entry
}

// assign output file names to the layers
func assignLayerNames(layers []plannedEntry) {
	names := make(map[*entryInfo]string)
	n := 0
	for i := range layers {
		// reuse the name if one is already assigned
		out, ok := names[layers[i].info]
		if !ok {
			out = fmt.Sprintf("layer%d.tar", n)
			n++
			names[layers[i].info] = out
		}
		layers[i].name = out
	}
}

// build a conversion plan from the parsed documents and the index information
func buildPlan(file string, index map[string]*entryInfo, docs *parsedDocs) (*plan, int) {
	// 1. from OCI
	// handle the case where oci-layout exists and index.json has been parsed
	_, hasLayout := index["oci-layout"]
	ociFailed := false
	if hasLayout && docs.ociIndex != nil {
		// return the plan if it can be converted
		if p, code := planFromOCI(file, index, docs); code == exitOK {
			return p, code
		}
		// if it cannot be converted, record the failure and fall back to the Docker format
		ociFailed = true
	}
	// 2. from Docker
	// the case where manifest.json has been parsed (error messages are also handled here)
	if docs.dockerManifest != nil || docs.dockerErr != nil {
		// return the plan if it can be converted
		return planFromDocker(index, docs)
	}
	// the case where conversion was not possible
	if !ociFailed {
		// show the following message when it is not an OCI conversion failure
		fmt.Fprintln(os.Stderr, "error: unsupported file format.")
	}
	return nil, exitFail
}

// --- OCI structs ---

type ociIndex struct {
	Manifests []ociDescriptor `json:"manifests"`
}

type ociDescriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Platform    *ociPlatform      `json:"platform,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type ociPlatform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant,omitempty"`
}

type ociManifest struct {
	Config      ociDescriptor     `json:"config"`
	Layers      []ociDescriptor   `json:"layers"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

func planFromOCI(file string, index map[string]*entryInfo, docs *parsedDocs) (*plan, int) {
	// fail if selecting the manifest had already failed
	if docs.ociManErr != nil {
		fmt.Fprintln(os.Stderr, docs.ociManErr)
		return nil, exitFail
	}

	// if the manifest has not been parsed yet, fetch its content and parse it
	if docs.ociManifest == nil {
		manData, found, rerr := entryContent(index, file, docs.ociManName)
		if rerr != nil {
			fmt.Fprintf(os.Stderr, "OCI: could not read manifest.\n")
			return nil, exitFail
		}
		if !found {
			fmt.Fprintf(os.Stderr, "OCI: manifest not found.\n")
			return nil, exitFail
		}
		var m ociManifest
		if err := json.Unmarshal(manData, &m); err != nil {
			fmt.Fprintf(os.Stderr, "OCI: could not parse manifest.\n")
			return nil, exitFail
		}
		docs.ociManifest = &m
	}

	// check the config
	configDigest := docs.ociManifest.Config.Digest
	configName, ok := blobPath(configDigest)
	if !ok {
		fmt.Fprintf(os.Stderr, "OCI: invalid digest for config (%s).\n", configDigest)
		return nil, exitFail
	}
	configInfo, ok := index[configName]
	if !ok {
		fmt.Fprintf(os.Stderr, "OCI: blob for config (%s) not found.\n", configDigest)
		return nil, exitFail
	}
	config := plannedEntry{info: configInfo, format: formatUnknown, name: "config.json"}

	// add the layers to the plan
	if len(docs.ociManifest.Layers) > maxLayers {
		fmt.Fprintf(os.Stderr, "OCI: number of layers exceeds the limit (%d).\n", maxLayers)
		return nil, exitFail
	}
	layers := make([]plannedEntry, len(docs.ociManifest.Layers))
	for i, l := range docs.ociManifest.Layers {
		name, ok := blobPath(l.Digest)
		if !ok {
			fmt.Fprintf(os.Stderr, "OCI: invalid digest for layer (%s).\n", l.Digest)
			return nil, exitFail
		}
		info, ok := index[name]
		if !ok {
			fmt.Fprintf(os.Stderr, "OCI: blob for layer (%s) not found.\n", l.Digest)
			return nil, exitFail
		}
		format, err := checkLayerFormat(l.MediaType, info.head)
		if err != nil {
			fmt.Fprintf(os.Stderr, "OCI: layer (%s): %s.\n", l.Digest, err)
			return nil, exitFail
		}
		layers[i] = plannedEntry{info: info, format: format}
	}
	assignLayerNames(layers)

	return &plan{repoTag: docs.ociTag, config: config, layers: layers}, exitOK
}

// select the target manifest from the OCI index
func resolveManifest(idx *ociIndex, platform, tag string, docs *parsedDocs) {
	desc, err := pickManifest(idx.Manifests, platform, tag)
	switch {
	case err != nil:
		docs.ociManErr = err
	case isIndexMediaType(desc.MediaType):
		// the selection points to yet another image index (nested)
		docs.ociManErr = errors.New("OCI: nested image indexes are not supported.")
	default:
		mn, ok := blobPath(desc.Digest)
		if !ok {
			docs.ociManErr = errors.New("OCI: invalid manifest digest.")
			return
		}
		docs.ociManName = mn
		docs.ociTag = desc.Annotations["org.opencontainers.image.ref.name"]
	}
}

// narrow the manifests down to one by platform and tag
func pickManifest(manifests []ociDescriptor, platform, tag string) (ociDescriptor, error) {
	if len(manifests) == 0 {
		return ociDescriptor{}, errors.New("OCI: manifest not found.")
	}
	wantOS, wantArch, wantVar := parsePlatform(platform)
	var matched []ociDescriptor
	for _, m := range manifests {
		if platform != "" {
			if m.Platform == nil {
				continue
			}
			if m.Platform.OS != wantOS || m.Platform.Architecture != wantArch {
				continue
			}
			if wantVar != "" && m.Platform.Variant != wantVar {
				continue
			}
		}
		if tag != "" && m.Annotations["org.opencontainers.image.ref.name"] != tag {
			continue
		}
		matched = append(matched, m)
	}
	if len(matched) == 1 {
		return matched[0], nil
	}
	return ociDescriptor{}, fmt.Errorf("OCI: could not select a manifest. "+
		"check the platform and tag.\n"+
		"- platforms in the image: %s\n"+
		"- tags in the image: %s.", platformChoices(manifests), tagChoices(manifests))
}

// build a string listing the platforms shown by the manifests in the index
func platformChoices(manifests []ociDescriptor) string {
	var ps []string
	for _, m := range manifests {
		if m.Platform == nil {
			continue
		}
		s := m.Platform.OS + "/" + m.Platform.Architecture
		if m.Platform.Variant != "" {
			s += "/" + m.Platform.Variant
		}
		ps = append(ps, s)
	}
	return strings.Join(ps, ", ")
}

// build a string listing the ref names (tags) held by the manifests in the index
func tagChoices(manifests []ociDescriptor) string {
	var ts []string
	for _, m := range manifests {
		if ref := m.Annotations["org.opencontainers.image.ref.name"]; ref != "" {
			ts = append(ts, ref)
		}
	}
	return strings.Join(ts, ", ")
}

func parsePlatform(s string) (os_, arch, variant string) {
	parts := strings.Split(s, "/")
	if len(parts) > 0 {
		os_ = parts[0]
	}
	if len(parts) > 1 {
		arch = parts[1]
	}
	if len(parts) > 2 {
		variant = parts[2]
	}
	return
}

func isIndexMediaType(mt string) bool {
	return mt == "application/vnd.oci.image.index.v1+json" ||
		mt == "application/vnd.docker.distribution.manifest.list.v2+json"
}

// convert a digest (algo:hex) into blobs/<algo>/<hex>
func blobPath(digest string) (string, bool) {
	algo, hex, ok := strings.Cut(digest, ":")
	if !ok || algo == "" || hex == "" {
		return "", false
	}
	return path.Join("blobs", algo, hex), true
}

// --- Docker structs ---

type dockerManifestEntry struct {
	Config   string   `json:"Config"`
	RepoTags []string `json:"RepoTags,omitempty"`
	Layers   []string `json:"Layers"`
}

type dockerManifest []dockerManifestEntry

// flatten and return the RepoTags of all entries
func dockerTags(man dockerManifest) []string {
	var tags []string
	for _, e := range man {
		tags = append(tags, e.RepoTags...)
	}
	return tags
}

// from multiple images and tags, choose the single entry and tag to output
func selectDockerImage(man dockerManifest, tag string) (dockerManifestEntry, string, error) {
	if tag != "" {
		for _, e := range man {
			for _, t := range e.RepoTags {
				if t == tag {
					return e, tag, nil
				}
			}
		}
		return dockerManifestEntry{}, "", fmt.Errorf("docker-archive: specified tag %q not found.\n"+
			"- tags in the image: %s.", tag, strings.Join(dockerTags(man), ", "))
	} else if len(man) == 1 {
		tag := ""
		if len(man[0].RepoTags) == 1 {
			tag = man[0].RepoTags[0]
		}
		return man[0], tag, nil
	}
	return dockerManifestEntry{}, "", fmt.Errorf("docker-archive: could not select an image. "+
		"specify a tag with -t <tag>.\n"+
		"- tags in the image: %s.", strings.Join(dockerTags(man), ", "))
}

// from manifest.json, narrow the target image down to a single manifest and a single tag, and set it in docs
func resolveDocker(content []byte, tag string, docs *parsedDocs) {
	var man dockerManifest
	if err := json.Unmarshal(content, &man); err != nil {
		docs.dockerErr = errors.New("docker-archive: could not parse manifest.")
		return
	}
	if len(man) == 0 {
		docs.dockerErr = errors.New("docker-archive: no valid manifest found.")
		return
	}
	entry, repoTag, err := selectDockerImage(man, tag)
	if err != nil {
		docs.dockerErr = err
		return
	}
	// reduce RepoTags to just the selected one as well
	if repoTag != "" {
		entry.RepoTags = []string{repoTag}
	} else {
		entry.RepoTags = nil
	}
	docs.dockerManifest = &entry
}

func planFromDocker(index map[string]*entryInfo, docs *parsedDocs) (*plan, int) {
	if docs.dockerErr != nil {
		fmt.Fprintln(os.Stderr, docs.dockerErr)
		return nil, exitFail
	}
	man := docs.dockerManifest
	repoTag := ""
	if len(man.RepoTags) == 1 {
		repoTag = man.RepoTags[0]
	}

	if len(man.Layers) > maxLayers {
		fmt.Fprintf(os.Stderr, "docker-archive: number of layers exceeds the limit (%d).\n", maxLayers)
		return nil, exitFail
	}

	// "conversion not needed" check: every layer has a .tar extension and is placed at the top level
	allTarTopLevel := len(man.Layers) > 0
	for _, l := range man.Layers {
		name := normName(l)
		if !isTopLevel(name) || !strings.HasSuffix(name, ".tar") {
			allTarTopLevel = false
			break
		}
	}
	if allTarTopLevel {
		fmt.Println("this image does not need conversion.")
		return nil, exitNotNeeded
	}

	// check the config
	configName := normName(man.Config)
	configInfo, ok := index[configName]
	if !ok {
		fmt.Fprintf(os.Stderr, "docker-archive: config (%s) not found.\n", man.Config)
		return nil, exitFail
	}
	config := plannedEntry{info: configInfo, format: formatUnknown, name: "config.json"}

	// add the layers to the plan
	layers := make([]plannedEntry, len(man.Layers))
	for i, l := range man.Layers {
		name := normName(l)
		info, ok := index[name]
		if !ok {
			fmt.Fprintf(os.Stderr, "docker-archive: layer (%s) not found.\n", name)
			return nil, exitFail
		}
		// Docker manifest.json has no per-layer mediaType, so detect it from the content
		format, err := checkLayerFormat("", info.head)
		if err != nil {
			fmt.Fprintf(os.Stderr, "docker-archive: layer (%s): %s.\n", name, err)
			return nil, exitFail
		}
		layers[i] = plannedEntry{info: info, format: format}
	}
	assignLayerNames(layers)

	return &plan{repoTag: repoTag, config: config, layers: layers}, exitOK
}

// --- compression format detection ---

var (
	magicGzip = []byte{0x1f, 0x8b}
	magicZstd = []byte{0x28, 0xb5, 0x2f, 0xfd}
)

// compression format of a layer
type entryFormat int

const (
	formatUnknown entryFormat = iota // unknown (unsupported)
	formatTar                        // uncompressed tar
	formatGzip                       // gzip
	formatZstd                       // zstd (unsupported)
)

// detect the layer compression format from the mediaType and the leading bytes; return an error for unsupported formats
func checkLayerFormat(mediaType string, head []byte) (entryFormat, error) {
	switch {
	case strings.Contains(mediaType, "zstd") || hasMagic(head, magicZstd):
		return formatZstd, errors.New("zstd-compressed layers are not supported")
	case strings.HasSuffix(mediaType, "+gzip") || strings.HasSuffix(mediaType, ".gzip") || hasMagic(head, magicGzip):
		return formatGzip, nil
	case mediaType == "" || strings.Contains(mediaType, "tar"):
		return formatTar, nil
	default:
		return formatUnknown, errors.New("unknown layer format")
	}
}

func hasMagic(data, magic []byte) bool {
	return len(data) >= len(magic) && string(data[:len(magic)]) == string(magic)
}

// report whether the name is at the top level (contains no "/")
func isTopLevel(name string) bool {
	return !strings.Contains(name, "/")
}

// normalize a tar entry name (strip a leading "./")
func normName(name string) string {
	return strings.TrimPrefix(name, "./")
}

// --- output ---

// scan the input once and write the target entries directly to the TAR
func writeOutput(outFile, inFile string, plan *plan) error {
	// build the output manifest.json
	layerNames := make([]string, len(plan.layers))
	for i := range plan.layers {
		layerNames[i] = plan.layers[i].name
	}
	var repoTags []string
	if plan.repoTag != "" {
		repoTags = []string{plan.repoTag}
	}
	manJSON, err := json.Marshal(dockerManifest{{
		Config:   plan.config.name,
		RepoTags: repoTags,
		Layers:   layerNames,
	}})
	if err != nil {
		return err
	}

	// create the output file
	f, err := os.Create(outFile)
	if err != nil {
		return err
	}
	defer f.Close()

	// prepare the writer
	var w io.Writer = f
	var gz *gzip.Writer
	if strings.HasSuffix(strings.ToLower(outFile), ".gz") {
		gz = gzip.NewWriter(f)
		w = gz
	}
	tw := tar.NewWriter(w)

	// write the manifest
	if err := writeTarBytes(tw, "manifest.json", manJSON); err != nil {
		return err
	}

	// write targets: input entry path -> output entry
	entries := append([]plannedEntry{plan.config}, plan.layers...)
	targets := make(map[string]plannedEntry, len(entries))
	for _, e := range entries {
		targets[e.info.path] = e
	}

	// scan the input image and output the target entries
	scanErr := eachEntry(inFile, func(name string, hdr *tar.Header, tr *tar.Reader) error {
		e, ok := targets[name]
		if !ok {
			return nil
		}
		switch e.format {
		case formatUnknown, formatTar:
			// write as is
			return writeTarStream(tw, e.name, e.info.size, tr)
		case formatGzip:
			// decompress to a temp file before writing
			path, size, err := gunzipToTemp(tr)
			if err != nil {
				return err
			}
			defer os.Remove(path)
			tf, err := os.Open(path)
			if err != nil {
				return err
			}
			defer tf.Close()
			return writeTarStream(tw, e.name, size, tf)
		default:
			return fmt.Errorf("unsupported format: %s", e.name)
		}
	})
	if scanErr != nil {
		return scanErr
	}

	// finish writing
	if err := tw.Close(); err != nil {
		return err
	}
	if gz != nil {
		if err := gz.Close(); err != nil {
			return err
		}
	}
	return f.Close()
}

// decompress a gzip stream to a temp file and return its path and size
func gunzipToTemp(r io.Reader) (path string, size int64, err error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return "", 0, fmt.Errorf("could not decompress gzip (%w)", err)
	}
	defer zr.Close()
	tmp, err := os.CreateTemp("", "oci2mtk-*.tar")
	if err != nil {
		return "", 0, err
	}
	size, err = io.Copy(tmp, zr)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp.Name())
		return "", 0, err
	}
	return tmp.Name(), size, nil
}

// write the content of r to the TAR as an entry of size bytes
func writeTarStream(tw *tar.Writer, name string, size int64, r io.Reader) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: size, Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err := io.Copy(tw, r)
	return err
}

func writeTarBytes(tw *tar.Writer, name string, data []byte) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}
