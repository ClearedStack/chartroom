// Command chartroom publishes, links, merges and retires catalog entries.
//
//	chartroom apply   apply one decision to a catalog file and print the change
//	chartroom run     run the worker against Muster, or alone against stand-ins
//	chartroom version print the version and the contract version
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"sort"
	"syscall"
	"time"

	"github.com/ClearedStack/chartroom/internal/catalog"
	"github.com/ClearedStack/chartroom/internal/cserr"
	"github.com/ClearedStack/chartroom/internal/decision"
	"github.com/ClearedStack/chartroom/internal/muster"
	"github.com/ClearedStack/chartroom/internal/plainforge"
	"github.com/ClearedStack/chartroom/internal/spool"
	"github.com/ClearedStack/chartroom/internal/strictjson"
	"github.com/ClearedStack/chartroom/internal/worker"
)

// Version is the component version from the naming registry.
const Version = "v0.1.0-draft"

// memoryLimit is the soft limit given to the Go runtime when the
// environment sets none. It sits under the 30 MiB budget to leave room for
// what the runtime does not count.
const memoryLimit = 24 << 20

// defaultTokenFile is where Kubernetes mounts the ServiceAccount token.
const defaultTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"

// Exit statuses.
const (
	exitOK           = 0
	exitInternal     = 1
	exitUsage        = 2
	exitParse        = 3
	exitNeedsSubtool = 4
	exitTimeout      = 5
)

func main() {
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(memoryLimit)
	}
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

const usage = `usage:
  chartroom apply -catalog FILE -decision FILE [-write]
  chartroom run -muster-url URL [-token-file FILE] [-spool DIR] [-once]
  chartroom run -standalone -catalog FILE -items DIR [-spool DIR]
  chartroom version
`

func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	// The contract version is owned outside this repository. If the
	// environment asks for one this build does not know, stop here.
	if v := getenv("CONTRACT_VERSION"); v != "" && v != catalog.ContractVersion {
		return fail(stderr, cserr.At(cserr.SchemaVersionMismatch, "contract"))
	}
	switch args[0] {
	case "apply":
		return cmdApply(args[1:], stdout, stderr)
	case "run":
		return cmdRun(args[1:], getenv, stdout, stderr)
	case "version":
		fmt.Fprintf(stdout, "chartroom %s contract %s\n", Version, catalog.ContractVersion)
		return exitOK
	case "help", "-h", "-help", "--help":
		fmt.Fprint(stdout, usage)
		return exitOK
	}
	fmt.Fprint(stderr, usage)
	return exitUsage
}

// fail prints a public error on one line and returns its exit status.
func fail(stderr io.Writer, err error) int {
	e, ok := cserr.From(err)
	if !ok {
		fmt.Fprintln(stderr, "chartroom: internal error")
		return exitInternal
	}
	fmt.Fprintln(stderr, e.Error())
	switch e.Reason.Code {
	case cserr.Parse:
		return exitParse
	case cserr.NeedsSubtool:
		return exitNeedsSubtool
	}
	return exitTimeout
}

func usageError(stderr io.Writer, msg string) int {
	fmt.Fprintln(stderr, "chartroom: "+msg)
	fmt.Fprint(stderr, usage)
	return exitUsage
}

func newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// readInput reads a file the user named. A file larger than limit is
// passed on one byte over, so the parser reports it as broken data.
func readInput(path string, limit int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, int64(limit)+1))
}

func cmdApply(args []string, stdout, stderr io.Writer) int {
	fs := newFlags("apply")
	catalogPath := fs.String("catalog", "", "")
	decisionPath := fs.String("decision", "", "")
	write := fs.Bool("write", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *catalogPath == "" || *decisionPath == "" {
		return usageError(stderr, "apply needs -catalog and -decision")
	}
	itemData, err := readInput(*decisionPath, decision.MaxItemBytes)
	if err != nil {
		return usageError(stderr, "the decision file cannot be read")
	}
	catalogData, err := readInput(*catalogPath, catalog.MaxBytes)
	if err != nil {
		return usageError(stderr, "the catalog file cannot be read")
	}
	change, after, err := decision.Run(catalogData, catalog.MaxBytes, itemData)
	if err != nil {
		return fail(stderr, err)
	}
	if *write && change.Changed {
		if err := replaceFile(*catalogPath, after.Encode()); err != nil {
			fmt.Fprintln(stderr, "chartroom: the catalog file cannot be written")
			return exitInternal
		}
	}
	_, _ = stdout.Write(change.Encode())
	return exitOK
}

// replaceFile swaps in new contents with a rename, so the catalog file is
// never left half written.
func replaceFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".chartroom-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if info, err := os.Stat(path); err == nil {
		_ = os.Chmod(tmp.Name(), info.Mode().Perm())
	}
	return os.Rename(tmp.Name(), path)
}

func cmdRun(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := newFlags("run")
	musterURL := fs.String("muster-url", getenv("MUSTER_URL"), "")
	tokenFile := fs.String("token-file", defaultTokenFile, "")
	spoolDir := fs.String("spool", spool.DefaultDir, "")
	once := fs.Bool("once", false, "")
	standalone := fs.Bool("standalone", false, "")
	catalogPath := fs.String("catalog", "", "")
	itemsDir := fs.String("items", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return usageError(stderr, "run was given arguments it does not know")
	}

	cfg := worker.DefaultConfig()
	log := slog.New(slog.NewJSONHandler(stderr, nil))
	sp, discarded, err := spool.Open(*spoolDir)
	if err != nil {
		fmt.Fprintln(stderr, "chartroom: "+spool.ErrUnusable.Error())
		return exitInternal
	}
	if discarded > 0 {
		log.Warn("spool_discarded", "code", string(cserr.Parse), "reason", cserr.MalformedJSON.Name, "count", discarded)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *standalone {
		if *catalogPath == "" || *itemsDir == "" {
			return usageError(stderr, "run -standalone needs -catalog and -items")
		}
		return runStandalone(ctx, cfg, sp, log, *catalogPath, *itemsDir, stdout, stderr)
	}
	if *musterURL == "" {
		return usageError(stderr, "run needs -muster-url, or -standalone")
	}
	client, err := muster.NewHTTP(*musterURL, *tokenFile, cfg.CallTimeout)
	if err != nil {
		return usageError(stderr, muster.ErrBadAddress.Error())
	}
	cfg.Once = *once
	log.Info("started", "version", Version, "contract", catalog.ContractVersion, "staged", sp.Len())
	worker.New(cfg, client, sp, worker.SystemClock{}, log).Run(ctx)
	log.Info("stopped", "staged", sp.Len())
	return exitOK
}

// standaloneReport is what a stand-alone run prints.
type standaloneReport struct {
	Items   []muster.Outcome `json:"items"`
	Catalog json.RawMessage  `json:"catalog"`
}

// runStandalone runs the whole worker loop with no other component: an
// in-memory Muster seeded from a directory of items, each as Muster would
// hold it (id, state, lane and the results of earlier workers), committing
// to an in-memory hub repository seeded from a catalog file. It prints
// what happened to each item, in the order the steps ended, and the
// catalog that results.
func runStandalone(ctx context.Context, cfg worker.Config, sp *spool.Spool, log *slog.Logger,
	catalogPath, itemsDir string, stdout, stderr io.Writer) int {
	catalogData, err := readInput(catalogPath, catalog.MaxBytes)
	if err != nil {
		return usageError(stderr, "the catalog file cannot be read")
	}
	names, err := filepath.Glob(filepath.Join(itemsDir, "*.json"))
	if err != nil || len(names) == 0 {
		return usageError(stderr, "the items directory has no .json files")
	}
	sort.Strings(names)

	hub := plainforge.NewMemory()
	_ = hub.WriteFile(ctx, muster.CatalogPath, catalogData, "seed")
	m := muster.NewMemory(hub, time.Now, muster.LeaseLength)
	for _, name := range names {
		data, err := readInput(name, decision.MaxItemBytes)
		if err != nil {
			return usageError(stderr, "an item file cannot be read")
		}
		var seed muster.Seed
		if err := json.Unmarshal(data, &seed); err != nil {
			return usageError(stderr, "an item file is not a JSON object with id, state, lane and results")
		}
		m.Seed(seed)
	}

	cfg.Once = true
	worker.New(cfg, m, sp, worker.SystemClock{}, log).Run(ctx)
	if errors.Is(ctx.Err(), context.Canceled) {
		return exitInternal
	}
	final, _ := hub.ReadFile(ctx, muster.CatalogPath)
	if !json.Valid(final) {
		final, _ = json.Marshal(string(final))
	}
	_, _ = stdout.Write(strictjson.Marshal(standaloneReport{Items: m.Outcomes(), Catalog: final}))
	return exitOK
}
