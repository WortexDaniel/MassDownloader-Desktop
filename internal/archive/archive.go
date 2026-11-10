// Package archive implements staged ZIP/RAR/7z extraction, matching archive_utils.py.
package archive

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const maxMembers = 50000
const maxUnpacked = 100 * 1024 * 1024 * 1024

func IsArchive(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.LastIndex(s, "."); i >= 0 {
		s = s[i+1:]
	}
	return s == "zip" || s == "rar" || s == "7z"
}
func safeMember(name string) (string, error) {
	name = strings.ReplaceAll(name, "\\", "/")
	if name == "" || strings.HasPrefix(name, "/") || strings.ContainsAny(name, "\x00:") {
		return "", fmt.Errorf("небезопасный путь в архиве: %q", name)
	}
	var parts []string
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return "", fmt.Errorf("выход из каталога в архиве: %q", name)
		}
		if part != "" && part != "." {
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		return "", fmt.Errorf("пустой путь в архиве")
	}
	return filepath.Join(parts...), nil
}
func tar(ctx context.Context, args ...string) ([]byte, error) {
	exe, err := exec.LookPath("tar")
	if err != nil {
		exe, err = exec.LookPath("bsdtar")
	}
	if err != nil {
		return nil, fmt.Errorf("для распаковки RAR/7z не найден tar.exe (bsdtar/libarchive)")
	}
	cmd := exec.CommandContext(ctx, exe, args...)
	hideWindow(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("распаковщик: %w: %s", err, out)
	}
	return out, nil
}
func Members(file string) ([]string, error) {
	return MembersContext(context.Background(), file)
}
func MembersContext(ctx context.Context, file string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !IsArchive(file) {
		return nil, fmt.Errorf("неподдерживаемый формат архива")
	}
	if strings.EqualFold(filepath.Ext(file), ".zip") {
		r, err := zip.OpenReader(file)
		if err != nil {
			return nil, err
		}
		defer r.Close()
		if len(r.File) > maxMembers {
			return nil, fmt.Errorf("слишком много файлов в архиве")
		}
		out := make([]string, 0, len(r.File))
		var total uint64
		for _, f := range r.File {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if f.Mode()&os.ModeSymlink != 0 {
				return nil, fmt.Errorf("символическая ссылка в ZIP запрещена: %s", f.Name)
			}
			if f.UncompressedSize64 > maxUnpacked-total {
				return nil, fmt.Errorf("распакованный ZIP превышает лимит 100 ГБ")
			}
			total += f.UncompressedSize64
			name, err := safeMember(f.Name)
			if err != nil {
				return nil, err
			}
			out = append(out, name)
		}
		return out, nil
	}
	ctx, cancel := context.WithTimeout(ctx, time.Hour)
	defer cancel()
	data, err := tar(ctx, "-tf", file)
	if err != nil {
		return nil, err
	}
	listing, err := tar(ctx, "-tvf", file)
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(string(listing), "\n") {
		if line != "" && line[0] != '-' && line[0] != 'd' {
			return nil, fmt.Errorf("необычный тип файла в архиве: %s", line)
		}
	}
	out := []string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name, err := safeMember(line)
		if err != nil {
			return nil, err
		}
		out = append(out, name)
		if len(out) > maxMembers {
			return nil, fmt.Errorf("слишком много файлов в архиве")
		}
	}
	return out, nil
}
func nextPath(file string) string {
	candidate := file
	for n := 2; ; n++ {
		if _, err := os.Lstat(candidate); os.IsNotExist(err) {
			return candidate
		}
		ext := filepath.Ext(file)
		candidate = strings.TrimSuffix(file, ext) + fmt.Sprintf("_%d", n) + ext
	}
}
func Extract(file string, intoParent bool, target string) (output string, members []string, err error) {
	return ExtractContext(context.Background(), file, intoParent, target)
}
func ExtractContext(ctx context.Context, file string, intoParent bool, target string) (output string, members []string, err error) {
	file, err = filepath.Abs(file)
	if err != nil {
		return
	}
	members, err = MembersContext(ctx, file)
	if err != nil {
		return
	}
	stage, e := os.MkdirTemp(filepath.Dir(file), ".extract-*")
	if e != nil {
		err = e
		return
	}
	defer os.RemoveAll(stage)
	if strings.EqualFold(filepath.Ext(file), ".zip") {
		r, e := zip.OpenReader(file)
		if e != nil {
			err = e
			return
		}
		defer r.Close()
		for i, f := range r.File {
			if err = ctx.Err(); err != nil {
				return
			}
			dest := filepath.Join(stage, members[i])
			if f.FileInfo().IsDir() || strings.HasSuffix(f.Name, "\\") {
				err = os.MkdirAll(dest, 0755)
				if err != nil {
					return
				}
				continue
			}
			if err = os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
				return
			}
			src, e := f.Open()
			if e != nil {
				err = e
				return
			}
			dst, e := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
			if e != nil {
				src.Close()
				err = e
				return
			}
			_, err = io.Copy(dst, &contextReader{ctx, io.LimitReader(src, int64(f.UncompressedSize64)+1)})
			closeErr := dst.Close()
			src.Close()
			if err == nil {
				err = closeErr
			}
			if err != nil {
				return
			}
		}
	} else {
		ctx, cancel := context.WithTimeout(ctx, time.Hour)
		defer cancel()
		_, err = tar(ctx, "-xf", file, "-C", stage, "--no-same-owner", "--no-same-permissions")
		if err != nil {
			return
		}
	}
	err = filepath.Walk(stage, func(p string, info os.FileInfo, e error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if e != nil {
			return e
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("символическая ссылка запрещена: %s", p)
		}
		return nil
	})
	if err != nil {
		return
	}
	if err = ctx.Err(); err != nil {
		return
	}
	if intoParent {
		if target == "" {
			target = filepath.Dir(file)
		}
		output, err = filepath.Abs(target)
		if err != nil {
			return
		}
		if err = os.MkdirAll(output, 0755); err != nil {
			return
		}
		var entries []os.DirEntry
		entries, err = os.ReadDir(stage)
		if err != nil {
			return
		}
		for _, entry := range entries {
			if err = os.Rename(filepath.Join(stage, entry.Name()), nextPath(filepath.Join(output, entry.Name()))); err != nil {
				return
			}
		}
		return
	}
	output = nextPath(strings.TrimSuffix(file, filepath.Ext(file)) + "_extracted")
	err = os.Rename(stage, output)
	return
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
