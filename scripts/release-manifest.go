// Run with go run ./scripts/release-manifest.go; only standard-library dependencies.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"
)

var versionPattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)[.](0|[1-9][0-9]*)[.](0|[1-9][0-9]*)$`)
var numberPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)
var repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

const maxArtifactSize = 512 << 20

type artifact struct {
	Arch      string `json:"arch"`
	Component string `json:"component"`
	URL       string `json:"https_url"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size_bytes"`
}

type manifest struct {
	Version     string     `json:"version"`
	OS          string     `json:"os"`
	MinProtocol int        `json:"min_agent_protocol"`
	Artifacts   []artifact `json:"artifacts"`
}

func releaseVersion(explicit string, getenv func(string) string, now time.Time) (string, error) {
	if explicit == "" {
		runID, attempt := getenv("GITHUB_RUN_ID"), getenv("GITHUB_RUN_ATTEMPT")
		if runID == "" && attempt == "" {
			// Local builds use nanoseconds; resolve once and pass the result to every build.
			runID, attempt = strconv.FormatInt(now.UnixNano(), 10), "0"
		}
		if !numberPattern.MatchString(runID) || !numberPattern.MatchString(attempt) {
			return "", fmt.Errorf("GITHUB_RUN_ID and GITHUB_RUN_ATTEMPT must both be canonical integers")
		}
		explicit = fmt.Sprintf("v%s.%s.%s", now.UTC().Format("20060102"), runID, attempt)
	}
	if !versionPattern.MatchString(explicit) {
		return "", fmt.Errorf("version must be vMAJOR.MINOR.PATCH without leading zeros: %q", explicit)
	}
	return explicit, nil
}

func inspectArtifact(dir, repository, version, arch, component string) (artifact, error) {
	filename := component + "-linux-" + arch
	a := artifact{Arch: arch, Component: component, URL: "https://github.com/" + repository + "/releases/download/" + version + "/" + filename}
	f, err := os.Open(filepath.Join(dir, filename))
	if err != nil {
		return a, fmt.Errorf("%s: %w", filename, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return a, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxArtifactSize {
		return a, fmt.Errorf("%s: expected a nonempty regular file of at most 512 MiB", filename)
	}
	h := sha256.New()
	a.Size, err = io.Copy(h, io.LimitReader(f, maxArtifactSize+1))
	if err != nil {
		return a, fmt.Errorf("%s: %w", filename, err)
	}
	if a.Size != info.Size() {
		return a, fmt.Errorf("%s: file size changed while hashing", filename)
	}
	a.SHA256 = hex.EncodeToString(h.Sum(nil))
	return a, nil
}

func run(args []string, getenv func(string) string, now time.Time, stdout io.Writer) error {
	flags := flag.NewFlagSet("release-manifest", flag.ContinueOnError)
	version := flags.String("version", "", "fixed vMAJOR.MINOR.PATCH; defaults to an automatic UTC version")
	versionOnly := flags.Bool("version-only", false, "print the version without reading artifacts")
	repository := flags.String("repository", getenv("GITHUB_REPOSITORY"), "GitHub OWNER/REPO")
	dir := flags.String("dir", ".", "directory containing all four Linux binaries")
	output := flags.String("output", "node-release.json", "manifest file, or - for JSON on stdout")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	resolved, err := releaseVersion(*version, getenv, now)
	if err != nil {
		return err
	}
	if *versionOnly {
		_, err = fmt.Fprintln(stdout, resolved)
		return err
	}
	if !repositoryPattern.MatchString(*repository) {
		return fmt.Errorf("repository must be GitHub OWNER/REPO")
	}
	m := manifest{Version: resolved, OS: "linux", MinProtocol: 1, Artifacts: make([]artifact, 0, 4)}
	for _, arch := range []string{"amd64", "arm64"} {
		for _, component := range []string{"mi-node", "xbctl"} {
			a, err := inspectArtifact(*dir, *repository, resolved, arch, component)
			if err != nil {
				return err
			}
			m.Artifacts = append(m.Artifacts, a)
		}
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if *output == "-" {
		_, err = stdout.Write(data)
		return err
	}
	// Validate all artifacts before replacing an existing manifest.
	return os.WriteFile(*output, data, 0644)
}

func main() {
	if err := run(os.Args[1:], os.Getenv, time.Now(), os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
