// SPDX-License-Identifier: Apache-2.0

// Package sandbox holds the pgx-free sandbox vocabulary shared by the daemon
// and the CLI: label constants, environment-derived configuration, and the
// pure validation of a SandboxSpec before any Docker call.
package sandbox

// Row and Docker labels, plus the fixed names the launcher and relay rely on.
// The row labels are written by the daemon from values it verified, never from
// anything the container reports.
const (
	// RowLabelSandbox marks a conversation/sandbox row; value "1".
	RowLabelSandbox = "rafiki/sandbox"
	// RowLabelID carries the sandbox row id.
	RowLabelID = "rafiki/sandbox-id"
	// RowLabelOwnerChild carries the owning child id (spawn block only).
	RowLabelOwnerChild = "rafiki/sandbox-owner-child"
	// RowLabelScope carries the spawn-block scope.
	RowLabelScope = "rafiki/sandbox-scope"
	// RowLabelCreatedBy carries the creating child id ("" for an operator).
	RowLabelCreatedBy = "rafiki/created-by"

	// DockerLabelSandbox is the container label naming the sandbox row.
	DockerLabelSandbox = "rafiki.sandbox"
	// DockerLabelChild is the container label naming the owning child.
	DockerLabelChild = "rafiki.child"

	// CredentialEnv is the env var the launcher reads the executor credential
	// from inside the container.
	CredentialEnv = "RAFIKI_EXECUTOR_CREDENTIAL"
	// DockerProxyName is the executor-proxy name the docker launcher advertises.
	DockerProxyName = "docker"
	// ContainerRelayDir is the relay directory mounted into the container.
	ContainerRelayDir = "/run/rafiki-relay"
	// ContainerNamePrefix prefixes every sandbox container name.
	ContainerNamePrefix = "rafiki-sbx-"
)
