// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"connectrpc.com/connect"
	"golang.org/x/sys/unix"

	executorpb "go.graveland.dev/rafiki/pkg/executorpb"
)

const (
	// treeChunkSize bounds one ReadTree stream chunk.
	treeChunkSize = 256 << 10
	// treeFreeFloor is the free space below which a WriteTree refuses to
	// start, or to continue, on the destination filesystem.
	treeFreeFloor int64 = 256 << 20
	// treeFreeCheckEvery is how many incoming tar-stream bytes may be consumed
	// between free-space re-checks. Stream bytes, not file bytes: a flood of
	// headers and padding costs disk work too, and counting only file content
	// would let an empty-file flood run unbounded.
	treeFreeCheckEvery int64 = 8 << 20
)

// treeRename renames oldpath to newpath. It is a package variable so tests can
// swap it, and it defaults to the raw rename(2) rather than os.Rename: Go's
// os.Rename carries a userspace guard that refuses rename(dir, existing-dir)
// with EEXIST even when POSIX replaces an empty directory, which is exactly the
// directory swap a tree transfer performs.
var (
	treeRename = func(oldpath, newpath string) error {
		return unix.Rename(oldpath, newpath)
	}
	// treeLink is os.Link, swappable so tests can drive the no-clobber publish
	// and its hard-link-unavailable fallback.
	treeLink      = os.Link
	treeFreeBytes = func(dir string) (int64, error) {
		var st unix.Statfs_t
		if err := unix.Statfs(dir, &st); err != nil {
			return 0, err
		}
		return int64(st.Bavail) * int64(st.Bsize), nil
	}
)

// treeOverwriteDenyPrefixes are the system paths a tree transfer may never
// replace. The check is prefix-based: the path itself or anything under it.
var treeOverwriteDenyPrefixes = []string{
	"/bin", "/boot", "/dev", "/etc", "/lib", "/lib32", "/lib64", "/proc",
	"/root", "/sbin", "/sys", "/usr", "/var", "/System", "/Library",
	"/Applications", "/private", "/Volumes", "/Users", "/home", "/opt",
}

// ReadTree streams a file or directory tree from this executor as a tar
// stream, prefixed by one TreeHeader message.
//
// It never follows symlinks out of the tree: a symlink is emitted verbatim,
// and sockets, devices and fifos are skipped because they cannot be
// transferred and their contents would be meaningless on the far side.
func (s *Server) ReadTree(
	ctx context.Context,
	req *connect.Request[executorpb.ReadTreeRequest],
	stream *connect.ServerStream[executorpb.ReadTreeResponse],
) error {
	p := req.Msg.GetPath()
	if !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("path %q must be absolute and clean", p))
	}

	fi, err := os.Lstat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return connect.NewError(connect.CodeNotFound, err)
		}
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
	switch {
	case fi.Mode()&os.ModeSymlink != 0:
		return connect.NewError(connect.CodeInvalidArgument, errors.New("path is a symlink"))
	case fi.IsDir(), fi.Mode().IsRegular():
		// The two shapes a tree can be.
	default:
		return connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("path %q is neither a regular file nor a directory", p))
	}

	if err := stream.Send(&executorpb.ReadTreeResponse{
		Msg: &executorpb.ReadTreeResponse_Header{
			Header: &executorpb.TreeHeader{IsDir: fi.IsDir()},
		},
	}); err != nil {
		return err
	}

	// The tar writer owns the walk and runs in its own goroutine; the request
	// goroutine only pumps bytes out. The pipe is what keeps a slow or failed
	// client from making the walk block forever: closing the reader unblocks
	// the writer with an error.
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		werr := treeWriteTar(ctx, tw, p, fi)
		if werr == nil {
			werr = tw.Close()
		} else {
			_ = tw.Close()
		}
		_ = pw.CloseWithError(werr)
	}()

	buf := make([]byte, treeChunkSize)
	for {
		n, rerr := pr.Read(buf)
		if n > 0 {
			if serr := stream.Send(&executorpb.ReadTreeResponse{
				Msg: &executorpb.ReadTreeResponse_Chunk{Chunk: buf[:n]},
			}); serr != nil {
				_ = pr.CloseWithError(serr)
				return serr
			}
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return connect.NewError(connect.CodeInternal, rerr)
		}
	}
}

// treeWriteTar writes the tar entries for root to tw. For a directory the root
// itself is not emitted — its entries are, relative to it; for a regular file
// exactly one entry named filepath.Base(root) is.
func treeWriteTar(ctx context.Context, tw *tar.Writer, root string, fi os.FileInfo) error {
	if !fi.IsDir() {
		return treeWriteFileEntry(tw, filepath.Base(root), root, fi)
	}
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if p == root {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		return treeWriteEntry(tw, filepath.ToSlash(rel), p, info)
	})
}

// treeWriteEntry emits one filesystem entry. Sockets, devices and fifos are
// silently skipped.
func treeWriteEntry(tw *tar.Writer, name, full string, fi os.FileInfo) error {
	mode := fi.Mode()
	switch {
	case mode.IsDir():
		return tw.WriteHeader(&tar.Header{
			Name:     name,
			Mode:     int64(mode.Perm()),
			Typeflag: tar.TypeDir,
		})
	case mode&os.ModeSymlink != 0:
		target, err := os.Readlink(full)
		if err != nil {
			return err
		}
		return tw.WriteHeader(&tar.Header{
			Name:     name,
			Mode:     int64(mode.Perm()),
			Typeflag: tar.TypeSymlink,
			Linkname: target,
		})
	case mode.IsRegular():
		return treeWriteFileEntry(tw, name, full, fi)
	default:
		return nil
	}
}

func treeWriteFileEntry(tw *tar.Writer, name, full string, fi os.FileInfo) error {
	f, err := treeOpenRead(full)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := tw.WriteHeader(&tar.Header{
		Name:     name,
		Mode:     int64(fi.Mode().Perm()),
		Size:     fi.Size(),
		Typeflag: tar.TypeReg,
	}); err != nil {
		return err
	}
	_, err = io.Copy(tw, f)
	return err
}

// treeOpenRead opens a file for reading without following a symlink at the
// final component and without leaking the descriptor to a spawned child.
func treeOpenRead(p string) (*os.File, error) {
	fd, err := unix.Open(p, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: p, Err: err}
	}
	return os.NewFile(uintptr(fd), p), nil
}

// WriteTree receives a file or directory tree on this executor.
//
// The tar is untrusted: the caller may be a sandbox that has been compromised,
// and this side is writing to a filesystem the daemon cares about. Every
// entry is validated before anything is created, extraction happens into a
// staging directory beside the destination, and the destination is only
// touched by the publish step once the whole transfer succeeded.
func (s *Server) WriteTree(
	_ context.Context,
	stream *connect.ClientStream[executorpb.WriteTreeRequest],
) (*connect.Response[executorpb.WriteTreeResponse], error) {
	if !stream.Receive() {
		if err := stream.Err(); err != nil {
			return nil, err
		}
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("WriteTree: stream ended before a start message"))
	}
	start := stream.Msg().GetStart()
	if start == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("WriteTree: the first message must be a start"))
	}

	dest := start.GetPath()
	if !filepath.IsAbs(dest) || filepath.Clean(dest) != dest {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("path %q must be absolute and clean", dest))
	}
	// max_bytes is optional on the wire precisely so that an explicit zero can
	// be refused: zero-means-unlimited is the trap, not the feature.
	if start.MaxBytes != nil && *start.MaxBytes <= 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("max_bytes must be greater than zero, got %d", *start.MaxBytes))
	}
	if start.GetOverwrite() {
		home, herr := os.UserHomeDir()
		if herr != nil {
			// An unknown home is not a reason to skip the guard's other rules,
			// only the home-specific ones — and never something to discard
			// silently.
			slog.Warn("executor: home directory unknown; the overwrite guard skips the home rules",
				"error", herr)
			home = ""
		}
		if err := checkOverwritePath(dest, s.opts.Root, home); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
	}

	parentDir := filepath.Dir(dest)
	if err := treeCheckFreeSpace(existingDir(parentDir)); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(parentDir, 0o755); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	staging, err := os.MkdirTemp(parentDir, ".rafiki-sync-")
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	// The destination's own mode is not carried by the stream (ReadTree never
	// emits the root), so the staging directory — which becomes the
	// destination — is given the conventional repository mode rather than
	// MkdirTemp's 0o700.
	if err := os.Chmod(staging, 0o755); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	// A failed transfer leaves nothing behind: every error path below runs
	// this, and after a successful publish staging no longer exists.
	defer os.RemoveAll(staging)

	var maxBytes int64
	if start.MaxBytes != nil {
		maxBytes = *start.MaxBytes
	}

	pr, pw := io.Pipe()
	type outcome struct {
		files int64
		bytes int64
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		files, total, xerr := extractTree(pr, staging, parentDir, maxBytes)
		_ = pr.CloseWithError(xerr)
		done <- outcome{files: files, bytes: total, err: xerr}
	}()

	for stream.Receive() {
		chunk := stream.Msg().GetChunk()
		if len(chunk) == 0 {
			continue
		}
		if _, werr := pw.Write(chunk); werr != nil {
			// The extractor stopped reading — either it aborted or it saw the
			// end of the archive. Stop feeding it.
			break
		}
	}
	streamErr := stream.Err()
	if streamErr != nil {
		_ = pw.CloseWithError(streamErr)
	} else {
		_ = pw.Close()
	}
	res := <-done
	if streamErr != nil {
		return nil, streamErr
	}
	if res.err != nil {
		return nil, res.err
	}

	// A file transfer carries exactly one entry, and it must be a regular file.
	staged := staging
	if !start.GetIsDir() {
		entries, rerr := os.ReadDir(staging)
		if rerr != nil {
			return nil, connect.NewError(connect.CodeInternal, rerr)
		}
		if len(entries) != 1 {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("a file transfer must contain exactly one entry, got %d", len(entries)))
		}
		staged = filepath.Join(staging, entries[0].Name())
		sfi, serr := os.Lstat(staged)
		if serr != nil {
			return nil, connect.NewError(connect.CodeInternal, serr)
		}
		if !sfi.Mode().IsRegular() {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("a file transfer must contain exactly one regular file, got a %s", sfi.Mode().Type()))
		}
	}

	// publish puts the staged tree in place. A directory is renamed (POSIX
	// rename replaces an empty destination directory); a file is hard-linked,
	// which cannot clobber — if the destination appeared after the checks
	// above, Link fails rather than silently replacing it.
	publish := func() error {
		if start.GetIsDir() {
			return treeRename(staged, dest)
		}
		return treePublishFile(staged, dest)
	}

	if !start.GetOverwrite() {
		fi, lerr := os.Lstat(dest)
		switch {
		case lerr == nil:
			if !start.GetIsDir() || !fi.IsDir() {
				return nil, connect.NewError(connect.CodeFailedPrecondition,
					errors.New("destination exists"))
			}
			ents, rerr := os.ReadDir(dest)
			if rerr != nil {
				return nil, connect.NewError(connect.CodeInternal, rerr)
			}
			if len(ents) != 0 {
				return nil, connect.NewError(connect.CodeFailedPrecondition,
					errors.New("destination exists"))
			}
		case os.IsNotExist(lerr):
			// The destination is clear.
		default:
			return nil, connect.NewError(connect.CodeInternal, lerr)
		}
		if err := publish(); err != nil {
			return nil, treeInternalErr(err)
		}
	} else {
		var oldAside string
		if _, lerr := os.Lstat(dest); lerr == nil {
			oldAside = filepath.Join(parentDir, ".rafiki-old-"+randomID())
			if err := treeRename(dest, oldAside); err != nil {
				return nil, connect.NewError(connect.CodeInternal, err)
			}
		} else if !os.IsNotExist(lerr) {
			return nil, connect.NewError(connect.CodeInternal, lerr)
		}
		if err := publish(); err != nil {
			if oldAside != "" {
				if rerr := treeRename(oldAside, dest); rerr != nil {
					return nil, connect.NewError(connect.CodeInternal,
						fmt.Errorf("publish the staged tree: %v (restoring the replaced tree also failed: %v; it is stranded at %s)",
							err, rerr, oldAside))
				}
			}
			return nil, treeInternalErr(err)
		}
		if oldAside != "" {
			// The new tree is already in place; a failure to remove the old one
			// is worth a warning, not an error that would misreport a
			// successful transfer.
			if rerr := removeTreeAll(oldAside); rerr != nil {
				slog.Warn("executor: replaced tree could not be removed and is stranded",
					"path", oldAside, "error", rerr)
			}
		}
	}

	return connect.NewResponse(&executorpb.WriteTreeResponse{
		Files: res.files,
		Bytes: res.bytes,
	}), nil
}

// treePublishFile puts a staged regular file at dest with a hard link, which
// cannot clobber: if dest appeared after the caller's checks, the link fails
// instead of replacing it. Filesystems that cannot hard-link fall back to a
// rename, but only after re-checking that dest is still absent — never to a
// clobbering rename.
func treePublishFile(staged, dest string) error {
	lerr := treeLink(staged, dest)
	switch {
	case lerr == nil:
		_ = os.Remove(staged)
		return nil
	case errors.Is(lerr, fs.ErrExist):
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("destination exists"))
	case errors.Is(lerr, unix.EPERM), errors.Is(lerr, unix.ENOTSUP), errors.Is(lerr, unix.EXDEV):
		if _, serr := os.Lstat(dest); serr == nil {
			return connect.NewError(connect.CodeFailedPrecondition, errors.New("destination exists"))
		} else if !os.IsNotExist(serr) {
			return serr
		}
		return treeRename(staged, dest)
	default:
		return lerr
	}
}

// treeInternalErr passes a Connect error through and classifies anything else
// as an internal failure, so a typed refusal from publish is not flattened to
// Internal by the wrapping at the call site.
func treeInternalErr(err error) error {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return err
	}
	return connect.NewError(connect.CodeInternal, err)
}

// removeTreeAll removes a tree even when it contains read-only directories: it
// chmods every directory to 0o700 first (write permission is what unlink needs)
// and then removes the whole tree.
func removeTreeAll(dir string) error {
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	return os.RemoveAll(dir)
}

// treeDirPerm is one directory's deferred chmod: extraction creates every
// directory 0o700 so an unprivileged mode never leaks mid-extraction, and the
// header's mode is applied at the end, deepest first.
type treeDirPerm struct {
	path string
	mode os.FileMode
}

// treeFileCounter counts the regular-file bytes written to it; that total is
// what the response reports. The resource guards count stream bytes instead —
// see treeStreamReader.
type treeFileCounter struct {
	dst   io.Writer
	total int64
}

func (w *treeFileCounter) Write(p []byte) (int, error) {
	n, err := w.dst.Write(p)
	w.total += int64(n)
	return n, err
}

// treeStreamReader wraps the incoming tar stream, counting every byte consumed
// — headers and padding included — and enforcing max_bytes and the periodic
// free-space floor on that total. Counting file content instead would let a
// flood of empty files, directories or symlinks do unbounded disk work while
// the counters never moved.
type treeStreamReader struct {
	r          io.Reader
	total      int64
	sinceCheck int64
	maxBytes   int64
	parentDir  string
}

func (s *treeStreamReader) Read(p []byte) (int, error) {
	if s.maxBytes > 0 {
		remaining := s.maxBytes - s.total
		if remaining <= 0 {
			return 0, connect.NewError(connect.CodeResourceExhausted,
				fmt.Errorf("tree stream exceeds max_bytes %d", s.maxBytes))
		}
		if int64(len(p)) > remaining {
			p = p[:remaining]
		}
	}
	n, err := s.r.Read(p)
	s.total += int64(n)
	s.sinceCheck += int64(n)
	if s.sinceCheck >= treeFreeCheckEvery {
		s.sinceCheck = 0
		if ferr := treeCheckFreeSpace(s.parentDir); ferr != nil {
			// 0 bytes, not n: io.ReadFull discards an error that arrives with a
			// read that filled the buffer, and tar.Reader reads headers through
			// io.ReadFull — so an error returned alongside the data would be
			// silently dropped and the transfer would carry on.
			return 0, ferr
		}
	}
	return n, err
}

// extractTree reads a tar stream and materialises it under staging. It returns
// the number of regular files and their total bytes. Malformed trees are
// CodeInvalidArgument; resource limits are CodeResourceExhausted.
func extractTree(r io.Reader, staging, parentDir string, maxBytes int64) (int64, int64, error) {
	sr := &treeStreamReader{r: r, maxBytes: maxBytes, parentDir: parentDir}
	tr := tar.NewReader(sr)
	w := &treeFileCounter{}
	var dirs []treeDirPerm
	var files int64

	for {
		hdr, herr := tr.Next()
		if herr == io.EOF {
			break
		}
		if herr != nil {
			return files, w.total, treeExtractErr(herr)
		}

		name, nerr := cleanEntryName(hdr.Name)
		if nerr != nil {
			return files, w.total, connect.NewError(connect.CodeInvalidArgument, nerr)
		}
		switch hdr.Typeflag {
		case tar.TypeReg, tar.TypeDir, tar.TypeSymlink:
			// The only entry kinds a tree may carry.
		case tar.TypeLink:
			return files, w.total, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("tar entry %q is a hard link, which is refused", hdr.Name))
		default:
			return files, w.total, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("tar entry %q has unsupported type %q", hdr.Name, hdr.Typeflag))
		}

		target, jerr := safeJoin(staging, name)
		if jerr != nil {
			return files, w.total, connect.NewError(connect.CodeInvalidArgument, jerr)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return files, w.total, treeExtractErr(err)
			}
			// Mkdir, not MkdirAll: an existing path here is a duplicate entry
			// or a symlink planted by an earlier entry, and neither is a
			// directory this transfer may adopt.
			if err := os.Mkdir(target, 0o700); err != nil {
				return files, w.total, treeExtractErr(err)
			}
			dirs = append(dirs, treeDirPerm{path: target, mode: os.FileMode(hdr.Mode).Perm()})
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return files, w.total, treeExtractErr(err)
			}
			// Verbatim: the link target is never resolved here, and may be
			// absolute or point outside the tree.
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return files, w.total, treeExtractErr(err)
			}
		default:
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return files, w.total, treeExtractErr(err)
			}
			// The mode goes to the kernel raw through unix.Open, so masking it
			// to 0o777 is what keeps a setuid/setgid/sticky tar mode from
			// becoming a setuid/setgid/sticky file. os.OpenFile would drop
			// those bits itself, which is exactly why it is not used: a guard
			// the standard library silently performs is a guard no test can
			// pin. O_EXCL refuses a path that already exists, symlink or not;
			// O_NOFOLLOW and O_CLOEXEC keep the final component from being a
			// symlink and the descriptor from leaking into a spawned child.
			perm := uint32(hdr.Mode) & 0o777
			fd, ferr := unix.Open(target,
				unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, perm)
			if ferr != nil {
				return files, w.total, treeExtractErr(ferr)
			}
			f := os.NewFile(uintptr(fd), target)
			w.dst = f
			_, cerr := io.Copy(w, tr)
			closeErr := f.Close()
			if cerr != nil {
				return files, w.total, treeExtractErr(cerr)
			}
			if closeErr != nil {
				return files, w.total, treeExtractErr(closeErr)
			}
			files++
		}
	}

	// Deepest first, so a parent's mode never locks a child out of the chmod.
	sort.Slice(dirs, func(i, j int) bool {
		return strings.Count(dirs[i].path, string(filepath.Separator)) >
			strings.Count(dirs[j].path, string(filepath.Separator))
	})
	for _, d := range dirs {
		if err := os.Chmod(d.path, d.mode); err != nil {
			return files, w.total, treeExtractErr(err)
		}
	}
	return files, w.total, nil
}

// treeExtractErr maps errors from extraction: a Connect error is passed
// through, a tar-format error is a malformed tree (CodeInvalidArgument), and
// anything else is an I/O or filesystem failure (CodeInternal).
func treeExtractErr(err error) error {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return err
	}
	if errors.Is(err, tar.ErrHeader) {
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewError(connect.CodeInternal, err)
}

// cleanEntryName validates a tar entry name and returns its slash-cleaned
// form. Absolute names, the empty name, and any name with a ".." element are
// refused.
func cleanEntryName(name string) (string, error) {
	if name == "" {
		return "", errors.New("tar entry has an empty name")
	}
	if path.IsAbs(name) {
		return "", fmt.Errorf("tar entry name %q is absolute", name)
	}
	clean := path.Clean(name)
	if clean == "." {
		return "", fmt.Errorf("tar entry name %q resolves to the destination root", name)
	}
	for _, part := range strings.Split(clean, "/") {
		if part == ".." {
			return "", fmt.Errorf("tar entry name %q escapes the destination", name)
		}
	}
	return clean, nil
}

// safeJoin resolves name under root, refusing any path that traverses an
// existing symlink. It Lstats every ancestor component: a symlink planted by
// an earlier entry must not become a way out of the staging directory, and
// resolving the whole path with one call would follow it.
func safeJoin(root, name string) (string, error) {
	parts := strings.Split(name, "/")
	cur := root
	for i, part := range parts {
		cur = filepath.Join(cur, part)
		if i == len(parts)-1 {
			break
		}
		fi, err := os.Lstat(cur)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "", err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("tar entry %q traverses the symlink %q", name, cur)
		}
	}
	return cur, nil
}

// checkOverwritePath refuses a destination whose replacement would be
// catastrophic. It is applied only when overwrite is set: without it a
// non-existent destination is required and nothing is replaced.
//
// Every rule is applied to the symlink-resolved path, so a link pointing into
// /etc is refused as /etc even though its own name is elsewhere. Two
// components is the floor, not three: /work/repo is a legitimate sandbox
// target, while /work and / are not targets at all.
func checkOverwritePath(target, root, home string) error {
	resolved, err := treeResolvePath(target)
	if err != nil {
		if errors.Is(err, errDanglingSymlink) {
			return fmt.Errorf("destination is a dangling symlink: %s", target)
		}
		return err
	}

	homeResolved := ""
	if home != "" {
		if r, herr := treeResolvePath(home); herr == nil {
			homeResolved = r
		} else {
			slog.Warn("executor: home directory is unusable for the overwrite guard",
				"home", home, "error", herr)
		}
	}
	rootResolved := ""
	if root != "" {
		if r, rerr := treeResolvePath(root); rerr == nil {
			rootResolved = r
		} else {
			rootResolved = filepath.Clean(root)
		}
	}
	tempResolved := filepath.Clean(os.TempDir())
	if r, terr := treeResolvePath(os.TempDir()); terr == nil {
		tempResolved = r
	}

	// An exemption base only means something when it is a real subtree: "/"
	// would exempt everything, and a base under a denied tree would exempt the
	// very tree the deny list exists to protect. The home base is stricter
	// than the others — see treeUsableHomeBase.
	if homeResolved != "" && !treeUsableHomeBase(homeResolved) {
		slog.Warn("executor: home directory is not a usable overwrite-guard exemption base",
			"home", homeResolved)
		homeResolved = ""
	}

	// Exceptions, checked before the deny list: a path strictly under the
	// user's home, under the process temp directory, or under the executor's
	// own root is a legitimate target even when a denied prefix would
	// otherwise cover it (/home, macOS's /private/var/folders, an executor
	// rooted under /usr).
	exemptFromDeny :=
		(treeUsableExemptionBase(tempResolved) && treeStrictDescendant(resolved, tempResolved)) ||
			(homeResolved != "" && treeStrictDescendant(resolved, homeResolved)) ||
			(treeUsableExemptionBase(rootResolved) && treeStrictDescendant(resolved, rootResolved))

	components := len(treeSplitPath(resolved))
	if components < 2 {
		return fmt.Errorf("refusing to overwrite %q: fewer than two path components", target)
	}

	if !exemptFromDeny {
		for _, prefix := range treeOverwriteDenyPrefixes {
			if treePathEqualOrUnder(resolved, prefix) {
				return fmt.Errorf("refusing to overwrite %q (%s): protected system path", target, resolved)
			}
		}
	}

	// Equality and ancestry are decided with os.SameFile, not string
	// comparison, so a case-insensitive filesystem or an alias cannot slip a
	// path past the root and home rules.
	if treeSameOrAncestor(resolved, rootResolved) {
		return fmt.Errorf("refusing to overwrite %q (%s): it is the executor root or an ancestor of it", target, resolved)
	}
	if homeResolved != "" && treeSameOrAncestor(resolved, homeResolved) {
		return fmt.Errorf("refusing to overwrite %q (%s): it is the home directory or an ancestor of it", target, resolved)
	}
	return nil
}

// errDanglingSymlink reports a path whose longest existing ancestor is a
// symlink that cannot be resolved.
var errDanglingSymlink = errors.New("path is a dangling symlink")

// treeResolvePath resolves the longest existing ancestor of p with
// EvalSymlinks and re-joins the non-existent tail, so a rule applied to the
// result sees through a symlink planted anywhere in the path. It returns
// errDanglingSymlink rather than the unresolved path when the existing
// ancestor is a symlink with a missing target: judging the link's own name
// instead of what it points at is exactly what the guard must not do.
func treeResolvePath(p string) (string, error) {
	clean := filepath.Clean(p)
	existing := clean
	var tail []string
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			break
		}
		tail = append([]string{filepath.Base(existing)}, tail...)
		existing = parent
	}
	resolved, err := filepath.EvalSymlinks(existing)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", errDanglingSymlink
		}
		return "", err
	}
	return filepath.Join(append([]string{resolved}, tail...)...), nil
}

// treeUsableExemptionBase reports whether a path may exempt its descendants
// from the deny list: it must be a real subtree, not "/" and not itself a
// deny-list root.
func treeUsableExemptionBase(base string) bool {
	if base == "" || base == string(filepath.Separator) {
		return false
	}
	for _, prefix := range treeOverwriteDenyPrefixes {
		if treePathEqual(base, prefix) {
			return false
		}
	}
	return true
}

// treeUsableHomeBase reports whether the home directory may exempt its
// descendants from the deny list. Homes legitimately live under /home, /Users
// and /root, so those prefixes are allowed; any OTHER denied tree is not a
// usable home base, because a deny-list tree reached through an alias (macOS's
// /etc -> /private/etc) would otherwise exempt the tree the deny list exists to
// protect.
func treeUsableHomeBase(base string) bool {
	if base == "" || base == string(filepath.Separator) {
		return false
	}
	for _, prefix := range treeOverwriteDenyPrefixes {
		switch prefix {
		case "/home", "/Users", "/root":
			continue
		}
		if treePathEqualOrUnder(base, prefix) {
			return false
		}
	}
	return true
}

// treeSplitPath splits a path into its non-empty, non-"." components.
func treeSplitPath(p string) []string {
	var out []string
	for _, part := range strings.Split(p, string(filepath.Separator)) {
		if part != "" && part != "." {
			out = append(out, part)
		}
	}
	return out
}

// treePathEqual reports whether two paths are the same path, comparing
// component-wise and case-insensitively so a deny-list root is not dodged by
// case on a case-sensitive filesystem.
func treePathEqual(a, b string) bool {
	ap, bp := treeSplitPath(a), treeSplitPath(b)
	if len(ap) != len(bp) {
		return false
	}
	for i := range ap {
		if !strings.EqualFold(ap[i], bp[i]) {
			return false
		}
	}
	return true
}

// treePathEqualOrUnder reports whether p is prefix itself or a path under it,
// component-wise and case-insensitively.
func treePathEqualOrUnder(p, prefix string) bool {
	pp, dp := treeSplitPath(p), treeSplitPath(prefix)
	if len(pp) < len(dp) {
		return false
	}
	for i := range dp {
		if !strings.EqualFold(pp[i], dp[i]) {
			return false
		}
	}
	return true
}

// treeStrictDescendant reports whether p is strictly under ancestor.
func treeStrictDescendant(p, ancestor string) bool {
	if ancestor == "" || p == ancestor {
		return false
	}
	ancestor = strings.TrimSuffix(ancestor, string(filepath.Separator))
	if ancestor == "" {
		return strings.HasPrefix(p, string(filepath.Separator))
	}
	return strings.HasPrefix(p, ancestor+string(filepath.Separator))
}

// treeSameOrAncestor reports whether resolved names protected itself or a
// directory containing it, comparing inodes with os.SameFile where both paths
// exist. A protected path that does not exist yet (a synthetic or uncreated
// executor root) falls back to a string comparison so it is still protected.
func treeSameOrAncestor(resolved, protected string) bool {
	if protected == "" {
		return false
	}
	protected = filepath.Clean(protected)
	rfi, rerr := os.Lstat(resolved)
	if rerr != nil {
		return resolved == protected ||
			strings.HasPrefix(protected, resolved+string(filepath.Separator))
	}
	for cur := protected; ; {
		if ci, err := os.Lstat(cur); err == nil && os.SameFile(rfi, ci) {
			return true
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	return false
}

// treeCheckFreeSpace refuses when dir's filesystem has less than
// treeFreeFloor free. It is the guard behind both the pre-write check and the
// periodic one during extraction.
func treeCheckFreeSpace(dir string) error {
	free, err := treeFreeBytes(dir)
	if err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	if free < treeFreeFloor {
		return connect.NewError(connect.CodeResourceExhausted,
			fmt.Errorf("executor has %d bytes free, below the %d byte floor", free, treeFreeFloor))
	}
	return nil
}

// existingDir walks up from dir to the nearest ancestor that exists, so a free
// space check can run before the destination's parents are created.
func existingDir(dir string) string {
	for {
		if _, err := os.Stat(dir); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return dir
		}
		dir = parent
	}
}
