// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"testing"

	"github.com/multigres/testkit/assert"
)

// unsetConvCacheBytes clears the variable for one test and restores the
// previous state afterwards. t.Setenv registers the restore; the Unsetenv is
// what makes the state genuinely unset rather than empty.
func unsetConvCacheBytes(t *testing.T) {
	t.Helper()
	t.Setenv(convCacheBytesEnv, "")
	if err := os.Unsetenv(convCacheBytesEnv); err != nil {
		t.Fatalf("unset %s: %v", convCacheBytesEnv, err)
	}
}

func TestConvCacheBytesDefault(t *testing.T) {
	c := assert.NewAborting(t)
	unsetConvCacheBytes(t)
	n, err := convCacheBytes()
	c.NoError(err, "unset is the documented default, not an error")
	c.Eq(int64(268435456), n, "the default is 256 MiB")
}

func TestConvCacheBytesRefusesNonPositive(t *testing.T) {
	c := assert.NewAborting(t)
	for _, v := range []string{"0", "-1", "abc"} {
		t.Setenv(convCacheBytesEnv, v)
		_, err := convCacheBytes()
		c.ErrorContains(err,
			fmt.Sprintf("RAFIKI_CONV_CACHE_BYTES must be a positive integer, got %q", v),
			"value %q", v)
	}
}

func TestConvCacheBytesParses(t *testing.T) {
	c := assert.NewAborting(t)
	t.Setenv(convCacheBytesEnv, "1048576")
	n, err := convCacheBytes()
	c.NoError(err, "a positive integer parses")
	c.Eq(int64(1048576), n, "the value is taken as given")
}
