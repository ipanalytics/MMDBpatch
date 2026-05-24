package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/ipanalytics/MMDBpatch/internal/patch"
	"github.com/maxmind/mmdbwriter"
	"github.com/oschwald/maxminddb-golang"
)

var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	var inputPath string
	var patchPath string
	var outputPath string
	var apply bool
	var jsonDiff bool
	var showVersion bool

	flag.StringVar(&inputPath, "input", "", "input MMDB path")
	flag.StringVar(&patchPath, "patch", "", "YAML patch file path")
	flag.StringVar(&outputPath, "output", "", "output MMDB path; requires -apply")
	flag.BoolVar(&apply, "apply", false, "write the patched MMDB instead of dry-run only")
	flag.BoolVar(&jsonDiff, "json", false, "print dry-run diff as JSON lines")
	flag.BoolVar(&showVersion, "version", false, "print version information")
	flag.Parse()

	if showVersion {
		fmt.Printf("mmdbpatch %s %s %s\n", version, commit, date)
		return nil
	}

	if inputPath == "" {
		return errors.New("-input is required")
	}
	if patchPath == "" {
		return errors.New("-patch is required")
	}
	if apply && outputPath == "" {
		return errors.New("-output is required when -apply is set")
	}

	patchFile, err := patch.LoadFile(patchPath)
	if err != nil {
		return err
	}
	if err := patchFile.Validate(); err != nil {
		return err
	}

	reader, err := maxminddb.Open(inputPath)
	if err != nil {
		return fmt.Errorf("open input MMDB: %w", err)
	}
	defer reader.Close()

	report, err := patch.Diff(reader, patchFile)
	if err != nil {
		return err
	}
	printReport(report, jsonDiff)

	if !apply {
		fmt.Fprintln(os.Stderr, "dry-run only; pass -apply and -output to write a patched MMDB")
		return nil
	}

	tree, err := mmdbwriter.Load(inputPath, writerOptions(reader))
	if err != nil {
		return fmt.Errorf("load MMDB for writing: %w", err)
	}
	if err := patch.Apply(tree, patchFile); err != nil {
		return err
	}

	out, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("create output MMDB: %w", err)
	}
	defer out.Close()

	if _, err := tree.WriteTo(out); err != nil {
		return fmt.Errorf("write output MMDB: %w", err)
	}

	fmt.Fprintf(os.Stderr, "wrote patched MMDB: %s\n", outputPath)
	return nil
}

func writerOptions(reader *maxminddb.Reader) mmdbwriter.Options {
	metadata := reader.Metadata
	return mmdbwriter.Options{
		BuildEpoch:              int64(metadata.BuildEpoch),
		DatabaseType:            metadata.DatabaseType,
		Description:             metadata.Description,
		IncludeReservedNetworks: true,
		IPVersion:               int(metadata.IPVersion),
		Languages:               metadata.Languages,
		RecordSize:              int(metadata.RecordSize),
	}
}

func printReport(report patch.Report, asJSON bool) {
	for _, change := range report.Changes {
		if asJSON {
			b, _ := json.Marshal(change)
			fmt.Println(string(b))
			continue
		}
		fmt.Printf("%s %s\n", change.Op, change.CIDR)
		fmt.Printf("  before: %s\n", compactJSON(change.Before))
		fmt.Printf("  after:  %s\n", compactJSON(change.After))
	}
	fmt.Fprintf(os.Stderr, "patches: %d, changed: %d\n", report.Total, report.Changed)
}

func compactJSON(v any) string {
	if v == nil {
		return "null"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}
