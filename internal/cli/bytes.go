// Copyright 2026 DataRobot, Inc. and its affiliates.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cli

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/pflag"
)

// bytesFlag is a pflag.Value for a size with an optional binary suffix, so --pack-size 64MiB
// reads the way a person would write it.
type bytesFlag struct {
	target *int64
}

var _ pflag.Value = bytesFlag{}

func (b bytesFlag) String() string {
	if b.target == nil || *b.target == 0 {
		return ""
	}

	return strconv.FormatInt(*b.target, 10)
}

func (b bytesFlag) Set(value string) error {
	parsed, err := ParseBytes(value)
	if err != nil {
		return err
	}

	*b.target = parsed

	return nil
}

func (bytesFlag) Type() string {
	return "size"
}

// ParseBytes reads a byte count with an optional suffix: 1024, 64KiB, 4MB, 2G.
func ParseBytes(value string) (int64, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, errors.New("empty size")
	}

	// Longest suffixes first: "KiB" has to be tried before "K" or "B".
	suffixes := []struct {
		suffix     string
		multiplier int64
	}{
		{suffix: "KiB", multiplier: 1 << 10},
		{suffix: "MiB", multiplier: 1 << 20},
		{suffix: "GiB", multiplier: 1 << 30},
		{suffix: "TiB", multiplier: 1 << 40},
		{suffix: "KB", multiplier: 1000},
		{suffix: "MB", multiplier: 1000 * 1000},
		{suffix: "GB", multiplier: 1000 * 1000 * 1000},
		{suffix: "K", multiplier: 1 << 10},
		{suffix: "M", multiplier: 1 << 20},
		{suffix: "G", multiplier: 1 << 30},
		{suffix: "T", multiplier: 1 << 40},
		{suffix: "B", multiplier: 1},
	}

	upper := strings.ToUpper(trimmed)

	for _, candidate := range suffixes {
		if !strings.HasSuffix(upper, strings.ToUpper(candidate.suffix)) {
			continue
		}

		digits := strings.TrimSpace(trimmed[:len(trimmed)-len(candidate.suffix)])

		number, err := strconv.ParseFloat(digits, 64)
		if err != nil {
			return 0, fmt.Errorf("cannot read %q as a size", value)
		}

		return int64(number * float64(candidate.multiplier)), nil
	}

	number, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("cannot read %q as a size", value)
	}

	return number, nil
}
