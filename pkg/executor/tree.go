// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	// treeFreeCheckEvery is how many regular-file bytes may be written
	// between free-space re-checks.
	treeFreeCheckEvery int64 = 8 << 20
)

// treeRename and treeFreeBytes are package variables so tests can swap them:
// the overwrite/restore path and the free-space floor are both hard to reach
// otherwise.
var (
	treeRename    = os.Rename
	treeFreeBytes = func(dir string) (int64, error) {
		var st unix.Statfs_t
		if err := unix.Statfs(dir, &st); err != nil {
			return 0, err
		}
		return int64(st.Bavail) * int64(st.Bsize), nil
	}
)

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
	f, err := os.Open(full)
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

// WriteTree receives a file or directory tree on this executor.
//
// The tar is untrusted: the caller may be a sandbox that has been compromised,
// and this side is writing to a filesystem the daemon cares about. Every
// entry is validated before anything is created, extraction happens into a
// staging directory beside the destination, and the destination is only
// touched by a rename once the whole transfer succeeded.
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
		home, _ := os.UserHomeDir()
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
	// A failed transfer leaves nothing behind: every error path below runs
	// this, and after a successful rename staging no longer exists.
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

	// The destination is touched only here, and only by renames.
	staged := staging
	if !start.GetIsDir() {
		// A file transfer stages the file inside the staging directory and
		// renames the file into place.
		entries, rerr := os.ReadDir(staging)
		if rerr != nil {
			return nil, connect.NewError(connect.CodeInternal, rerr)
		}
		if len(entries) != 1 {
			return nil, connect.NewError(connect.CodeInvalidArgument,
				fmt.Errorf("a file transfer must contain exactly one entry, got %d", len(entries)))
		}
		staged = filepath.Join(staging, entries[0].Name())
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
			// os.Rename refuses to replace an existing directory outright, so
			// the empty one is removed first. It holds nothing, so nothing is
			// lost if the rename then fails.
			if err := os.Remove(dest); err != nil {
				return nil, connect.NewError(connect.CodeInternal, err)
			}
		case os.IsNotExist(lerr):
			// The destination is clear.
		default:
			return nil, connect.NewError(connect.CodeInternal, lerr)
		}
		if err := treeRename(staged, dest); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
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
		if err := treeRename(staged, dest); err != nil {
			if oldAside != "" {
				if rerr := treeRename(oldAside, dest); rerr != nil {
					return nil, connect.NewError(connect.CodeInternal,
						fmt.Errorf("rename staged tree into place: %v (restoring the old tree also failed: %v)", err, rerr))
				}
			}
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		if oldAside != "" {
			_ = os.RemoveAll(oldAside)
		}
	}

	return connect.NewResponse(&executorpb.WriteTreeResponse{
		Files: res.files,
		Bytes: res.bytes,
	}), nil
}

// treeDirPerm is one directory's deferred chmod: extraction creates every
// directory 0o700 so an unprivileged mode never leaks mid-extraction, and the
// header's mode is applied at the end, deepest first.
type treeDirPerm struct {
	path string
	mode os.FileMode
}

// treeExtractWriter counts regular-file bytes as they are written, enforces
// max_bytes, and re-checks free space every treeFreeCheckEvery bytes.
type treeExtractWriter struct {
	dst        io.Writer
	total      int64
	sinceCheck int64
	maxBytes   int64
	parentDir  string
}

func (w *treeExtractWriter) Write(p []byte) (int, error) {
	if w.maxBytes > 0 && w.total+int64(len(p)) > w.maxBytes {
		return 0, connect.NewError(connect.CodeResourceExhausted,
			fmt.Errorf("tree exceeds max_bytes %d", w.maxBytes))
	}
	n, err := w.dst.Write(p)
	w.total += int64(n)
	w.sinceCheck += int64(n)
	if w.sinceCheck >= treeFreeCheckEvery {
		w.sinceCheck = 0
		if ferr := treeCheckFreeSpace(w.parentDir); ferr != nil {
			return n, ferr
		}
	}
	return n, err
}

// extractTree reads a tar stream and materialises it under staging. It returns
// the number of regular files and their total bytes. Every validation failure
// is CodeInvalidArgument; resource limits are CodeResourceExhausted.
func extractTree(r io.Reader, staging, parentDir string, maxBytes int64) (int64, int64, error) {
	tr := tar.NewReader(r)
	w := &treeExtractWriter{parentDir: parentDir, maxBytes: maxBytes}
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
			// The mode goes to the kernel raw through unix.Open, so the mask
			// here is what actually keeps a setuid/setgid/sticky tar mode from
			// becoming a setuid/setgid/sticky file. os.OpenFile would drop
			// those bits for us, which is exactly why it is not used: a guard
			// the standard library silently performs is a guard no test can
			// pin. O_EXCL refuses a path that already exists, symlink or not.
			perm := uint32(hdr.Mode) & 0o777 &^ (0o4000 | 0o2000 | 0o1000)
			fd, ferr := unix.Open(target, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, perm)
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

// treeExtractErr passes a Connect error through and classifies anything else
// as a malformed tree.
func treeExtractErr(err error) error {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return err
	}
	return connect.NewError(connect.CodeInvalidArgument, err)
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
// Two components is the floor, not three: /work/repo is a legitimate sandbox
// target, while /work and / are not targets at all. root and home are refused
// with their ancestors, because replacing a directory that contains either
// takes the executor's own working tree with it.
func checkOverwritePath(target, root, home string) error {
	clean := filepath.Clean(target)
	components := 0
	for _, part := range strings.Split(clean, string(filepath.Separator)) {
		if part != "" && part != "." {
			components++
		}
	}
	if components < 2 {
		return fmt.Errorf("refusing to overwrite %q: fewer than two path components", target)
	}
	switch clean {
	case "/", "/bin", "/boot", "/dev", "/etc", "/home", "/lib", "/opt",
		"/proc", "/root", "/sbin", "/sys", "/tmp", "/usr", "/var",
		"/Users", "/System", "/Library", "/Applications":
		return fmt.Errorf("refusing to overwrite %q: protected top-level path", target)
	}
	for _, protected := range []string{root, home} {
		if protected == "" {
			continue
		}
		protected = filepath.Clean(protected)
		if clean == protected || strings.HasPrefix(protected, clean+string(filepath.Separator)) {
			return fmt.Errorf("refusing to overwrite %q: it is %q or an ancestor of it", target, protected)
		}
	}
	return nil
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
