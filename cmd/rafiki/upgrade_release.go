// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"strings"
)

// smokeCheck runs the staged binary and requires it to print wantTag.
func smokeCheck(ctx context.Context, r runner, binary, wantTag string) error {
	stdout, _, err := r.Run(ctx, nil, binary, "--version")
	if err == nil {
		for _, field := range strings.Fields(string(stdout)) {
			if field == wantTag {
				return nil
			}
		}
	}
	reports := fmt.Errorf("%s reports %q, want %s", binary, strings.TrimSpace(string(stdout)), wantTag)
	if err != nil {
		return fmt.Errorf("%w: %w", reports, err)
	}
	return reports
}
