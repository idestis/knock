package app

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/idestis/knock/cli/internal/ui"
)

// releaseRepo is where releases are published (the same feed the app's Sparkle uses).
const releaseRepo = "idestis/knock"

type ghRelease struct {
	TagName string `json:"tag_name"`
	HTMLURL string `json:"html_url"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

// runUpdate replaces this binary with the latest release from GitHub,
// after checking its SHA-256 against the release's checksums.txt.
func runUpdate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	check := fs.Bool("check", false, "only report whether an update exists")
	force := fs.Bool("force", false, "update even from a dev build or to the same version")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		return errors.New("usage: knock update [--check] [--force]")
	}

	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	rel, err := latestRelease(ctx)
	if err != nil {
		return err
	}
	latest := strings.TrimPrefix(rel.TagName, "v")
	current := strings.TrimPrefix(Version, "v")
	newer := compareVersions(latest, current) > 0

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	brew := installedByHomebrew(exe)

	switch {
	case current == "dev" && !*force:
		ui.Say("This is a development build. The latest release is %s.", latest)
		ui.Note("Run knock update --force to replace it with the release.")
		return nil
	case !newer && !*force:
		ui.Say("You're on the latest version, %s.", current)
		return nil
	case *check && brew:
		ui.Say("Knock %s is available (you have %s). Run brew upgrade idestis/tap/knock to install it.", latest, current)
		return nil
	case *check:
		ui.Say("Knock %s is available (you have %s). Run knock update to install it.", latest, current)
		return nil
	case brew:
		// Replacing a file inside the Cellar would leave Homebrew out of step with what's installed.
		ui.Say("Knock %s is available (you have %s).", latest, current)
		ui.Note("You installed knock with Homebrew, so run brew upgrade idestis/tap/knock.")
		return nil
	}

	name := fmt.Sprintf("knock_%s_%s_%s.tar.gz", latest, runtime.GOOS, runtime.GOARCH)
	var archiveURL, sumsURL string
	for _, a := range rel.Assets {
		switch a.Name {
		case name:
			archiveURL = a.URL
		case "checksums.txt":
			sumsURL = a.URL
		}
	}
	if archiveURL == "" {
		return fmt.Errorf("Release %s has no build for %s/%s", latest, runtime.GOOS, runtime.GOARCH)
	}
	if sumsURL == "" {
		return fmt.Errorf("Release %s has no checksums.txt, so I won't install it", latest)
	}

	ui.Say("Downloading Knock %s…", latest)
	want, err := checksumFor(ctx, sumsURL, name)
	if err != nil {
		return err
	}
	archive, err := download(ctx, archiveURL)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(archive)
	if hex.EncodeToString(sum[:]) != want {
		return errors.New("The download doesn't match the release checksum, so I didn't install it")
	}
	bin, err := extractKnock(archive)
	if err != nil {
		return err
	}

	if err := replaceExecutable(exe, bin); err != nil {
		if errors.Is(err, os.ErrPermission) {
			return fmt.Errorf("I can't write to %s. Run sudo knock update, or reinstall knock in a folder you own", filepath.Dir(exe))
		}
		return err
	}
	ui.Say("Updated to Knock %s.", latest)
	ui.Note("Release notes: %s", rel.HTMLURL)
	return nil
}

func latestRelease(ctx context.Context) (ghRelease, error) {
	var rel ghRelease
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/"+releaseRepo+"/releases/latest", nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "knock-cli/"+Version)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return rel, errors.New("Can't reach GitHub to check for updates")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return rel, errors.New("There's no release yet")
	}
	if resp.StatusCode != http.StatusOK {
		return rel, fmt.Errorf("GitHub answered %d while checking for updates", resp.StatusCode)
	}
	return rel, json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel)
}

func download(ctx context.Context, url string) ([]byte, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("User-Agent", "knock-cli/"+Version)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download failed: GitHub answered %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 200<<20))
}

func checksumFor(ctx context.Context, url, name string) (string, error) {
	b, err := download(ctx, url)
	if err != nil {
		return "", err
	}
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			return strings.ToLower(f[0]), nil
		}
	}
	return "", fmt.Errorf("checksums.txt doesn't list %s", name)
}

// extractKnock returns the `knock` binary from a release tarball.
func extractKnock(archive []byte) ([]byte, error) {
	gz, err := gzip.NewReader(strings.NewReader(string(archive)))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, errors.New("the release archive has no knock binary")
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg && filepath.Base(h.Name) == "knock" {
			return io.ReadAll(io.LimitReader(tr, 200<<20))
		}
	}
}

// replaceExecutable swaps exe for bin atomically (rename in the same folder).
func replaceExecutable(exe string, bin []byte) error {
	dir := filepath.Dir(exe)
	tmp, err := os.CreateTemp(dir, ".knock-update-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(bin); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), exe)
}

// installedByHomebrew reports whether exe (symlinks resolved) lives in a Homebrew Cellar.
func installedByHomebrew(exe string) bool {
	return strings.Contains(filepath.ToSlash(exe), "/Cellar/knock/")
}

// compareVersions compares dotted versions numerically: 1 if a > b, -1 if a < b.
func compareVersions(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(strings.SplitN(pa[i], "-", 2)[0])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(strings.SplitN(pb[i], "-", 2)[0])
		}
		if x != y {
			if x > y {
				return 1
			}
			return -1
		}
	}
	return 0
}
