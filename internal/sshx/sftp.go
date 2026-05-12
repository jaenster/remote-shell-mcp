package sshx

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/pkg/sftp"
)

// sftpClient lazily opens a per-session SFTP client. It is dropped when the
// session reconnects or is closed; the next call rebuilds it on the new
// transport. Reconnect.Reconnect / clearClient call dropSFTP.
func (s *Session) sftpClient() (*sftp.Client, error) {
	s.sftpMu.Lock()
	defer s.sftpMu.Unlock()
	if s.sftp != nil {
		return s.sftp, nil
	}
	client, err := s.getClient()
	if err != nil {
		return nil, err
	}
	sc, err := sftp.NewClient(client)
	if err != nil {
		return nil, fmt.Errorf("open sftp: %w", err)
	}
	s.sftp = sc
	return sc, nil
}

func (s *Session) dropSFTP() {
	s.sftpMu.Lock()
	c := s.sftp
	s.sftp = nil
	s.sftpMu.Unlock()
	if c != nil {
		_ = c.Close()
	}
}

// closeSFTP is retained for callers that used the previous global API.
func (s *Session) closeSFTP() { s.dropSFTP() }

type FileEntry struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	Mode    string    `json:"mode"`
	ModTime time.Time `json:"mod_time"`
	IsDir   bool      `json:"is_dir"`
	Target  string    `json:"target,omitempty"`
}

func (s *Session) FileList(path string) ([]FileEntry, error) {
	c, err := s.sftpClient()
	if err != nil {
		return nil, err
	}
	entries, err := c.ReadDir(path)
	if err != nil {
		return nil, err
	}
	out := make([]FileEntry, 0, len(entries))
	for _, e := range entries {
		fe := FileEntry{
			Name:    e.Name(),
			Size:    e.Size(),
			Mode:    e.Mode().String(),
			ModTime: e.ModTime(),
			IsDir:   e.IsDir(),
		}
		if e.Mode()&os.ModeSymlink != 0 {
			target, _ := c.ReadLink(filepath.Join(path, e.Name()))
			fe.Target = target
		}
		out = append(out, fe)
	}
	return out, nil
}

func (s *Session) FileStat(path string) (*FileEntry, error) {
	c, err := s.sftpClient()
	if err != nil {
		return nil, err
	}
	st, err := c.Stat(path)
	if err != nil {
		return nil, err
	}
	return &FileEntry{
		Name:    st.Name(),
		Size:    st.Size(),
		Mode:    st.Mode().String(),
		ModTime: st.ModTime(),
		IsDir:   st.IsDir(),
	}, nil
}

// maxFileReadBytes caps a single FileRead call to keep a malicious or buggy
// client from OOMing the daemon by asking for, say, 100 GiB.
const maxFileReadBytes = 64 * 1024 * 1024

func (s *Session) FileRead(path string, offset int64, length int64) ([]byte, error) {
	c, err := s.sftpClient()
	if err != nil {
		return nil, err
	}
	f, err := c.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return nil, err
		}
	}
	if length > 0 {
		if length > maxFileReadBytes {
			length = maxFileReadBytes
		}
		buf := make([]byte, length)
		n, err := io.ReadFull(f, buf)
		if err == io.ErrUnexpectedEOF || err == io.EOF {
			return buf[:n], nil
		}
		if err != nil {
			return nil, err
		}
		return buf, nil
	}
	return io.ReadAll(io.LimitReader(f, maxFileReadBytes))
}

func (s *Session) FileWrite(path string, data []byte, append bool) error {
	c, err := s.sftpClient()
	if err != nil {
		return err
	}
	flags := os.O_WRONLY | os.O_CREATE
	if append {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	f, err := c.OpenFile(path, flags)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(data)
	return err
}

func (s *Session) FileDelete(path string) error {
	c, err := s.sftpClient()
	if err != nil {
		return err
	}
	return c.Remove(path)
}

func (s *Session) FileMkdir(path string, recursive bool) error {
	c, err := s.sftpClient()
	if err != nil {
		return err
	}
	if recursive {
		return c.MkdirAll(path)
	}
	return c.Mkdir(path)
}

func (s *Session) FileChmod(path string, mode os.FileMode) error {
	c, err := s.sftpClient()
	if err != nil {
		return err
	}
	return c.Chmod(path, mode)
}

func (s *Session) FileRename(from, to string) error {
	c, err := s.sftpClient()
	if err != nil {
		return err
	}
	return c.Rename(from, to)
}

func (s *Session) Upload(localPath, remotePath string) (int64, error) {
	c, err := s.sftpClient()
	if err != nil {
		return 0, err
	}
	in, err := os.Open(localPath)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	out, err := c.Create(remotePath)
	if err != nil {
		return 0, err
	}
	defer out.Close()
	return io.Copy(out, in)
}

func (s *Session) Download(remotePath, localPath string) (int64, error) {
	c, err := s.sftpClient()
	if err != nil {
		return 0, err
	}
	in, err := c.Open(remotePath)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	out, err := os.Create(localPath)
	if err != nil {
		return 0, err
	}
	defer out.Close()
	return io.Copy(out, in)
}
