package main

import (
	"errors"
	"flag"
	"strings"
	"testing"
)

func TestHelpIsNotAnError(t *testing.T) {
	if err := run([]string{"-h"}); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("-h: %v, want flag.ErrHelp (exit 0)", err)
	}
}

func TestInvalidFlagsAreRejected(t *testing.T) {
	for _, args := range [][]string{
		{"-log-format", "xml"},
		{"-max-ingress-kbps", "-1"},
		{"-ingress-burst-kib", "0"},
		{"-max-rtx-kbps", "0"},
		{"-audio-fec-k", "49"},
		{"-audio-fec-k", "-1"},
		{"-max-room-size", "129"},
		{"-max-room-size", "1"},
		{"-binding-timeout", "1s"},
		{"-udp-batch", "0"},
		{"-control-rate", "0"},
		{"-control-rate", "NaN"},
		{"-max-conns", "-1"},
		{"-max-registers-per-sec", "-1"},
		{"stray"},
	} {
		err := run(args)
		if err == nil || errors.Is(err, flag.ErrHelp) || strings.Contains(err.Error(), "listen") {
			t.Errorf("%v: got %v, want a validation error", args, err)
		}
	}
}
