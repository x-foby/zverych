// Command mutation runs mutation tests for this module.
//
// go-mutesting is used only as a mutant generator (--no-exec). Its own runner
// overwrites source files in place and therefore tests mutants strictly one at a
// time; here every mutant is tested through `go test -overlay`, which swaps the
// file inside the compiler only. The working tree is never modified and mutants run
// on all CPUs. Each run also uses -vet=off (vet does not decide whether a mutant is
// killed) and -failfast (the first failing test is enough).
//
// Usage:
//
//	go run ./scripts/mutation [flags] [packages]   (default: ./..., except this tool)
//
// Install the generator: go install github.com/avito-tech/go-mutesting/cmd/go-mutesting@latest
// (requires Go >= 1.27; with Go 1.25–1.26 use @466f39c).
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type status string

const (
	killed   status = "killed"
	timedOut status = "timeout" // counts as killed: the tests did not pass
	survived status = "survived"
	skipped  status = "skipped" // the mutant did not compile
)

type mutant struct {
	Package  string        `json:"package"`
	File     string        `json:"file"` // relative to the module root
	Mutator  string        `json:"mutator"`
	Checksum string        `json:"checksum"`
	Status   status        `json:"status"`
	Elapsed  time.Duration `json:"elapsed_ns"`
	Diff     string        `json:"diff,omitempty"` // survivors only
	Output   string        `json:"output,omitempty"`

	orig, mutated string // absolute paths
}

type pkg struct{ ImportPath, Dir string }

var (
	reMutator = regexp.MustCompile(`^Mutator (\S+)$`)
	reSaved   = regexp.MustCompile(`^Save mutation into "(.+)" with checksum ([0-9a-f]{32})$`)
	reTmpDir  = regexp.MustCompile(`^Save mutations into "(.+)"$`)
	reSuffix  = regexp.MustCompile(`\.\d+$`)
)

func main() {
	jobs := flag.Int("j", runtime.NumCPU(), "mutants tested in parallel")
	timeout := flag.Duration("timeout", 10*time.Second, "test timeout per mutant (go test -timeout)")
	out := flag.String("out", ".mutation", "report directory")
	minMSI := flag.Float64("min-msi", 0, "exit 1 if the total score is below this value, e.g. 0.85")
	bin := flag.String("bin", "", "go-mutesting binary (default: PATH, then $(go env GOPATH)/bin)")
	blacklist := flag.String("blacklist", "", "file with MD5 checksums of mutants to ignore (equivalent mutants)")
	keep := flag.Bool("keep", false, "keep generated mutants and overlays")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	code, err := run(ctx, options{*jobs, *timeout, *out, *minMSI, *bin, *blacklist, *keep}, flag.Args())
	if err != nil {
		fmt.Fprintln(os.Stderr, "mutation:", err)
	}
	os.Exit(code)
}

type options struct {
	jobs      int
	timeout   time.Duration
	out       string
	minMSI    float64
	bin       string
	blacklist string
	keep      bool
}

func run(ctx context.Context, o options, patterns []string) (int, error) {
	root, err := goOutput(ctx, "", "list", "-m", "-f", "{{.Dir}}")
	if err != nil {
		return 2, err
	}
	if o.bin, err = findGenerator(ctx, o.bin); err != nil {
		return 2, err
	}
	pkgs, err := listPackages(ctx, root, patterns)
	if err != nil {
		return 2, err
	}
	work, err := os.MkdirTemp("", "mutation-")
	if err != nil {
		return 2, err
	}
	var cleanup []string
	defer func() {
		if o.keep {
			fmt.Fprintln(os.Stderr, "kept:", strings.Join(append(cleanup, work), " "))
			return
		}
		for _, d := range append(cleanup, work) {
			os.RemoveAll(d)
		}
	}()

	start := time.Now()
	mutants, tmpDirs, err := generate(ctx, o, root, work, pkgs)
	cleanup = tmpDirs
	if err != nil {
		return 2, err
	}
	genTime := time.Since(start)
	fmt.Fprintf(os.Stderr, "generated %d mutants in %d packages (%s); testing with %d workers, timeout %s\n",
		len(mutants), len(pkgs), genTime.Round(time.Millisecond), o.jobs, o.timeout)

	if err := test(ctx, o, root, work, mutants); err != nil {
		return 130, err
	}
	total := time.Since(start)

	if err := writeReports(o.out, mutants); err != nil {
		return 2, err
	}
	msi := printSummary(mutants, pkgs, total)
	fmt.Printf("\nreports: %s (all mutants), %s (surviving mutants with diffs)\n",
		filepath.Join(o.out, "report.json"), filepath.Join(o.out, "survived.txt"))
	if o.minMSI > 0 && msi < o.minMSI {
		return 1, fmt.Errorf("mutation score %.4f is below -min-msi %.4f", msi, o.minMSI)
	}
	return 0, nil
}

func goOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	b, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("go %s: %w: %s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(string(b)), nil
}

func findGenerator(ctx context.Context, bin string) (string, error) {
	if bin != "" {
		return bin, nil
	}
	if p, err := exec.LookPath("go-mutesting"); err == nil {
		return p, nil
	}
	gopath, err := goOutput(ctx, "", "env", "GOPATH")
	if err == nil {
		p := filepath.Join(strings.SplitN(gopath, string(os.PathListSeparator), 2)[0], "bin", "go-mutesting")
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", errors.New("go-mutesting not found: go install github.com/avito-tech/go-mutesting/cmd/go-mutesting@latest " +
		"(Go >= 1.27; with Go 1.25–1.26 use @466f39c)")
}

func listPackages(ctx context.Context, root string, patterns []string) ([]pkg, error) {
	self := len(patterns) == 0
	if self {
		patterns = []string{"./..."}
	}
	lines, err := goOutput(ctx, root, append([]string{"list", "-f", "{{.ImportPath}}\t{{.Dir}}"}, patterns...)...)
	if err != nil {
		return nil, err
	}
	var pkgs []pkg
	for l := range strings.SplitSeq(lines, "\n") {
		ip, dir, _ := strings.Cut(l, "\t")
		if self && strings.HasSuffix(ip, "/scripts/mutation") {
			continue // do not mutate the tool itself
		}
		pkgs = append(pkgs, pkg{ip, dir})
	}
	return pkgs, nil
}

// generate runs go-mutesting --no-exec for every package (in parallel) and collects
// the mutant files it writes. It never touches the source tree.
func generate(ctx context.Context, o options, root, work string, pkgs []pkg) ([]*mutant, []string, error) {
	var (
		mu      sync.Mutex
		all     = make([][]*mutant, len(pkgs))
		tmpDirs []string
		errs    []error
		wg      sync.WaitGroup
	)
	for i, p := range pkgs {
		wg.Go(func() {
			ms, tmp, err := generatePackage(ctx, o, root, filepath.Join(work, fmt.Sprintf("gen%d", i)), p)
			mu.Lock()
			defer mu.Unlock()
			if tmp != "" {
				tmpDirs = append(tmpDirs, tmp)
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", p.ImportPath, err))
			}
			all[i] = ms
		})
	}
	wg.Wait()
	var mutants []*mutant
	for _, ms := range all {
		mutants = append(mutants, ms...)
	}
	return mutants, tmpDirs, errors.Join(errs...)
}

func generatePackage(ctx context.Context, o options, root, cwd string, p pkg) ([]*mutant, string, error) {
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		return nil, "", err
	}
	args := []string{"--no-exec", "--debug", "--verbose", "--do-not-remove-tmp-folder"}
	if o.blacklist != "" {
		bl, err := filepath.Abs(o.blacklist)
		if err != nil {
			return nil, "", err
		}
		args = append(args, "--blacklist="+bl)
	}
	cmd := exec.CommandContext(ctx, o.bin, append(args, p.Dir)...)
	cmd.Dir = cwd // go-mutesting drops report.json into its working directory
	outb, err := cmd.CombinedOutput()
	var mutants []*mutant
	var tmp, mutator string
	sc := bufio.NewScanner(bytes.NewReader(outb))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if m := reTmpDir.FindStringSubmatch(line); m != nil {
			tmp = m[1]
		} else if m := reMutator.FindStringSubmatch(line); m != nil {
			mutator = m[1]
		} else if m := reSaved.FindStringSubmatch(line); m != nil {
			orig := reSuffix.ReplaceAllString(strings.TrimPrefix(m[1], tmp+"/"), "")
			rel, _ := filepath.Rel(root, orig)
			mutants = append(mutants, &mutant{Package: p.ImportPath, File: rel, Mutator: mutator, Checksum: m[2],
				orig: orig, mutated: m[1]})
		}
	}
	if err != nil {
		return nil, tmp, fmt.Errorf("go-mutesting: %w\n%s", err, outb)
	}
	if tmp == "" {
		return nil, "", fmt.Errorf("unexpected go-mutesting output:\n%s", outb)
	}
	return mutants, tmp, nil
}

// test runs `go test` for every mutant with the mutated file swapped in via -overlay.
func test(ctx context.Context, o options, root, work string, mutants []*mutant) error {
	queue := make(chan int)
	var done atomic.Int64
	var wg sync.WaitGroup
	for range max(o.jobs, 1) {
		wg.Go(func() {
			for i := range queue {
				testMutant(ctx, o, root, filepath.Join(work, fmt.Sprintf("overlay%d.json", i)), mutants[i])
				if n := done.Add(1); n%25 == 0 || int(n) == len(mutants) {
					fmt.Fprintf(os.Stderr, "\r%d/%d", n, len(mutants))
				}
			}
		})
	}
feed:
	for i := range mutants {
		select {
		case queue <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(queue)
	wg.Wait()
	fmt.Fprintln(os.Stderr)
	return ctx.Err()
}

func testMutant(ctx context.Context, o options, root, overlay string, m *mutant) {
	start := time.Now()
	defer func() { m.Elapsed = time.Since(start) }()
	b, _ := json.Marshal(map[string]map[string]string{"Replace": {m.orig: m.mutated}})
	if err := os.WriteFile(overlay, b, 0o600); err != nil {
		m.Status, m.Output = skipped, err.Error()
		return
	}
	// The hard limit also covers compilation, which -timeout does not.
	cctx, cancel := context.WithTimeout(ctx, o.timeout+time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, "go", "test", "-overlay="+overlay, "-vet=off", "-failfast", "-count=1",
		"-timeout="+o.timeout.String(), m.Package)
	cmd.Dir = root
	outb, err := cmd.CombinedOutput()
	out := string(outb)
	switch {
	case err == nil:
		m.Status = survived
		m.Diff = diff(m)
	case strings.Contains(out, "[build failed]") || strings.Contains(out, "[setup failed]"):
		m.Status, m.Output = skipped, tail(out, 2000)
	case strings.Contains(out, "panic: test timed out") || cctx.Err() != nil:
		m.Status = timedOut
	default:
		m.Status = killed
	}
}

func diff(m *mutant) string {
	b, _ := exec.Command("diff", "-u", "--label", m.File, "--label", m.File+" (mutant)", m.orig, m.mutated).Output()
	return string(b)
}

func tail(s string, n int) string {
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}

func writeReports(dir string, mutants []*mutant) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	sorted := append([]*mutant(nil), mutants...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].File < sorted[j].File })
	b, err := json.MarshalIndent(sorted, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "report.json"), b, 0o644); err != nil {
		return err
	}
	var s strings.Builder
	for _, m := range sorted {
		if m.Status == survived {
			fmt.Fprintf(&s, "### %s  %s  %s\n%s\n", m.File, m.Mutator, m.Checksum, m.Diff)
		}
	}
	return os.WriteFile(filepath.Join(dir, "survived.txt"), []byte(s.String()), 0o644)
}

// printSummary prints a per-package table and returns the total score.
func printSummary(mutants []*mutant, pkgs []pkg, total time.Duration) float64 {
	type row struct {
		counts  map[status]int
		elapsed time.Duration
	}
	rows := map[string]*row{}
	sum := &row{counts: map[status]int{}}
	for _, m := range mutants {
		r := rows[m.Package]
		if r == nil {
			r = &row{counts: map[status]int{}}
			rows[m.Package] = r
		}
		r.counts[m.Status]++
		r.elapsed += m.Elapsed
		sum.counts[m.Status]++
	}
	score := func(c map[status]int) float64 {
		k := c[killed] + c[timedOut]
		if k+c[survived] == 0 {
			return 1
		}
		return float64(k) / float64(k+c[survived])
	}
	fmt.Printf("%-40s %7s %8s %8s %8s %7s %8s\n", "package", "killed", "timeout", "survived", "skipped", "score", "cpu")
	for _, p := range pkgs {
		r := rows[p.ImportPath]
		if r == nil {
			continue
		}
		c := r.counts
		fmt.Printf("%-40s %7d %8d %8d %8d %6.1f%% %7.0fs\n", p.ImportPath, c[killed], c[timedOut], c[survived], c[skipped],
			100*score(c), r.elapsed.Seconds())
	}
	c := sum.counts
	msi := score(c)
	fmt.Printf("%-40s %7d %8d %8d %8d %6.1f%%   wall %s\n", "TOTAL", c[killed], c[timedOut], c[survived], c[skipped],
		100*msi, total.Round(time.Second))
	return msi
}
