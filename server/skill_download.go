package server

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"
)

var (
	errSkillArchiveInvalid = errors.New("技能目录无法打包")
	errSkillArchiveLimit   = errors.New("技能压缩包超出限制")
)

// 留出 multipart 表单头空间，使导出的 ZIP 可通过现有上传接口重新安装。
const maxSkillDownloadBytes = maxSkillZipBytes - 1024

type skillArchiveBuffer struct{ bytes.Buffer }

func (b *skillArchiveBuffer) Write(p []byte) (int, error) {
	if len(p) > maxSkillDownloadBytes-b.Len() {
		return 0, fmt.Errorf("%w：压缩后须小于 20 MB", errSkillArchiveLimit)
	}
	return b.Buffer.Write(p)
}

type skillArchiveReader struct {
	ctx context.Context
	r   io.Reader
}

func (r skillArchiveReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// 遍历时也使用非阻塞打开，防止目录被并发替换为 FIFO 后卡住请求。
type skillArchiveFS struct {
	root *os.Root
	ctx  context.Context
}

func (f skillArchiveFS) Open(name string) (fs.File, error) {
	if err := f.ctx.Err(); err != nil {
		return nil, err
	}
	return openSkillArchiveFile(f.root, filepath.FromSlash(name))
}

func (f skillArchiveFS) ReadDir(name string) ([]fs.DirEntry, error) {
	file, err := f.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%w：目录发生变化，请重试", errSkillArchiveInvalid)
	}
	entries, err := file.(*os.File).ReadDir(maxSkillEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(entries) > maxSkillEntries {
		return nil, fmt.Errorf("%w：文件和目录不能超过 %d 项", errSkillArchiveLimit, maxSkillEntries)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, nil
}

func (s *Server) fsDownloadSkill(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	name := r.PathValue("name")
	if !validSkillName(name) {
		writeErr(w, http.StatusBadRequest, "技能名称无效")
		return
	}
	archive, err := buildSkillArchive(r.Context(), s.skillDir, name)
	if err != nil {
		switch {
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			writeErr(w, http.StatusRequestTimeout, "技能打包已取消或超时")
		case errors.Is(err, errSkillArchiveLimit):
			writeErr(w, http.StatusRequestEntityTooLarge, err.Error())
		case errors.Is(err, errSkillArchiveInvalid):
			writeErr(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, fs.ErrNotExist):
			writeErr(w, http.StatusNotFound, "技能不存在或打包期间被移除")
		default:
			writeErr(w, http.StatusInternalServerError, "技能打包失败，请检查文件读取权限后重试")
		}
		return
	}
	// 完整打包后才发送下载头，避免错误被当成一个成功的残缺 ZIP。
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name + ".zip"}))
	http.ServeContent(w, r, name+".zip", time.Time{}, bytes.NewReader(archive))
}

func buildSkillArchive(ctx context.Context, skillDir, name string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	library, err := os.OpenRoot(skillDir)
	if err != nil {
		return nil, err
	}
	defer library.Close()
	info, err := library.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w：技能根目录不能是链接或普通文件", errSkillArchiveInvalid)
	}
	root, err := library.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	opened, err := root.Stat(".")
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, opened) {
		return nil, fmt.Errorf("%w：目录发生变化，请重试", errSkillArchiveInvalid)
	}
	manifest, err := root.Lstat("SKILL.md")
	if err != nil || !manifest.Mode().IsRegular() {
		return nil, fmt.Errorf("%w：缺少常规文件 SKILL.md", errSkillArchiveInvalid)
	}

	// 先校验完整目录和总量，再读文件压缩。Root 将实际读取限制在该技能内。
	type entry struct {
		path string
		info fs.FileInfo
	}
	var entries []entry
	var total int64
	manifestSeen := false
	err = fs.WalkDir(skillArchiveFS{root, ctx}, ".", func(rel string, d fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if rel == "." {
			return nil
		}
		archivePath := rel
		if d.IsDir() {
			archivePath += "/"
		}
		if _, msg := skillRelPath(archivePath); msg != "" {
			return fmt.Errorf("%w：不支持的文件路径 %q", errSkillArchiveInvalid, rel)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("%w：不支持链接或特殊文件 %q", errSkillArchiveInvalid, rel)
		}
		if len(entries) >= maxSkillEntries {
			return fmt.Errorf("%w：文件和目录不能超过 %d 项", errSkillArchiveLimit, maxSkillEntries)
		}
		if !info.IsDir() {
			if rel == "SKILL.md" {
				manifestSeen = true
			}
			if info.Size() > maxSkillFileBytes {
				return fmt.Errorf("%w：单个文件不能超过 20 MB", errSkillArchiveLimit)
			}
			total += info.Size()
			if total > maxSkillTotalBytes {
				return fmt.Errorf("%w：文件总大小不能超过 100 MB", errSkillArchiveLimit)
			}
		}
		entries = append(entries, entry{rel, info})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !manifestSeen {
		return nil, fmt.Errorf("%w：SKILL.md 发生变化，请重试", errSkillArchiveInvalid)
	}

	var out skillArchiveBuffer
	zw := zip.NewWriter(&out)
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		header, err := zip.FileInfoHeader(e.info)
		if err != nil {
			return nil, err
		}
		header.Name = name + "/" + e.path
		if e.info.IsDir() {
			header.Name += "/"
			if _, err := zw.CreateHeader(header); err != nil {
				return nil, err
			}
			continue
		}
		err = func() error {
			file, err := openSkillArchiveFile(root, filepath.FromSlash(e.path))
			if err != nil {
				return err
			}
			defer file.Close()
			current, err := file.Stat()
			if err != nil {
				return err
			}
			if !current.Mode().IsRegular() || !os.SameFile(e.info, current) || current.Size() != e.info.Size() {
				return fmt.Errorf("%w：文件发生变化，请重试", errSkillArchiveInvalid)
			}
			header.Method = zip.Deflate
			writer, err := zw.CreateHeader(header)
			if err != nil {
				return err
			}
			n, err := io.Copy(writer, io.LimitReader(skillArchiveReader{ctx, file}, e.info.Size()+1))
			if err != nil {
				return err
			}
			if n != e.info.Size() {
				return fmt.Errorf("%w：文件发生变化，请重试", errSkillArchiveInvalid)
			}
			return nil
		}()
		if err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
