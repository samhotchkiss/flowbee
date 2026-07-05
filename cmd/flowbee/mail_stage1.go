package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/samhotchkiss/flowbee/internal/mailstage1"
)

func runMailStage1(args []string) error {
	fs := flag.NewFlagSet("mail-stage1", flag.ContinueOnError)
	inputPath := fs.String("input", "-", "JSON or JSONL measurement input; '-' reads stdin")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected args: %s", strings.Join(fs.Args(), " "))
	}

	var in io.Reader = os.Stdin
	var f *os.File
	if *inputPath != "-" {
		var err error
		f, err = os.Open(*inputPath)
		if err != nil {
			return fmt.Errorf("open input: %w", err)
		}
		defer f.Close()
		in = f
	}
	return scoreMailStage1(in, os.Stdout)
}

func scoreMailStage1(in io.Reader, out io.Writer) error {
	raw, err := io.ReadAll(in)
	if err != nil {
		return fmt.Errorf("read input: %w", err)
	}
	records, err := decodeMailStage1Records(raw)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(out)
	for _, report := range mailstage1.ProcessBatch(mailstage1.NewProcessor(), records) {
		if err := enc.Encode(report); err != nil {
			return fmt.Errorf("write report: %w", err)
		}
	}
	return nil
}

func decodeMailStage1Records(raw []byte) ([]mailstage1.Input, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty input")
	}
	switch raw[0] {
	case '[':
		var records []mailstage1.Input
		if err := json.Unmarshal(raw, &records); err != nil {
			return nil, fmt.Errorf("decode JSON array: %w", err)
		}
		return records, nil
	case '{':
		var record mailstage1.Input
		if err := json.Unmarshal(raw, &record); err == nil {
			return []mailstage1.Input{record}, nil
		}
	}

	var records []mailstage1.Input
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var record mailstage1.Input
		if err := json.Unmarshal(line, &record); err != nil {
			return nil, fmt.Errorf("decode JSONL line %d: %w", len(records)+1, err)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan JSONL: %w", err)
	}
	if len(records) == 0 {
		return nil, fmt.Errorf("empty input")
	}
	return records, nil
}
