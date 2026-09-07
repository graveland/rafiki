// SPDX-License-Identifier: Apache-2.0

// Package rafiki carries the module-root assets that must live beside their
// //go:embed directive. Embed patterns resolve relative to the package
// directory and cannot cross into a parent, so the core skill corpus — kept
// at skills/<name>/SKILL.md in the repo root for review like code (see
// docs/plans/2026-09-03-db-backed-skills-design.md) — can only be embedded
// by a package that lives at the repository root.
package rafiki

import "embed"

// Skills is the embedded core skill corpus: one SKILL.md per directory under
// skills/. It is a CARRIER, not a serving tier — cmd/rafikid reconciles it
// into the daemon's skills store at startup, and everything downstream reads
// the store.
//
//go:embed skills
var Skills embed.FS
