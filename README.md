# pi-controller

A daemon that hosts pi children running in `--mode rpc`, multiplexes their event streams to multiple concurrent clients, and exposes a small control plane over a Unix domain socket.

See `tasks/pi-controller-protocol.md` for the wire protocol spec.
See `tasks/2026-05-25-implementation-plan-daemon.md` for the implementation plan.
