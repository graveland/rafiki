// SPDX-License-Identifier: Apache-2.0

package main

import "github.com/spf13/cobra"

// newDbCmd is the `db` group: the verbs that reach the database installed on
// the daemon's own host. They live together because they share one fact — where
// that database is — which is a question only the daemon's host can answer:
// RAFIKI_DB (including the embedded server's data directory) is not something a
// remote client can resolve.
func newDbCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "db",
		Short: "Operate on rafikid's database on this host: psql, backup, restore",
		Long: `Verbs that reach rafikid's database from the machine rafikid runs on.

A database has no remote face here: RAFIKI_DB names either a server this host
can dial or the private embedded server, whose socket is a directory inside its
data directory. That is why these verbs belong to rafikid, and why the rafiki
client's own "rafiki db" passthrough forwards to them instead of dialling.

Subcommands: psql (open a client on the database), backup (write a dump, or a
physical copy of the embedded data directory), restore (put one back — it needs
--force, because it destroys what the backup covers).`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	// --db is persistent, so every verb in the group resolves the database the
	// same way and a subcommand never declares a flag of the same name.
	cmd.PersistentFlags().String("db", dbDSNDefault(), "postgres DSN (or RAFIKI_DB, then RAFIKI_TEST_DSN)")

	cmd.AddCommand(newDbPsqlCmd())
	cmd.AddCommand(newDbBackupCmd())
	cmd.AddCommand(newDbRestoreCmd())

	return cmd
}
