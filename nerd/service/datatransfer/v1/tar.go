package v1datatransfer

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dchest/safefile"
	"github.com/pkg/errors"
)

//tardir archives the given directory and writes bytes to w.
func tardir(dir string, w io.Writer) (err error) {
	tw := tar.NewWriter(w)
	err = filepath.Walk(dir, func(path string, fi os.FileInfo, err error) error {
		if fi.Mode().IsDir() {
			return nil
		}

		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return errors.Wrapf(err, "failed to determine path '%s' relative to '%s'", path, dir)
		}

		f, err := os.Open(path)
		defer f.Close()
		if err != nil {
			return errors.Wrapf(err, "failed to open file '%s'", rel)
		}

		err = tw.WriteHeader(&tar.Header{
			Name:    rel,
			Mode:    int64(fi.Mode()),
			ModTime: fi.ModTime(),
			Size:    fi.Size(),
		})
		if err != nil {
			if err == io.ErrClosedPipe {
				return err
			}
			return errors.Wrapf(err, "failed to write tar header for '%s'", rel)
		}

		n, err := io.Copy(tw, f)
		// fmt.Printf("%v %v\n", path, n)
		if err != nil {
			if err == io.ErrClosedPipe {
				return err
			}
			return errors.Wrapf(err, "failed to write tar file for '%s'", rel)
		}

		if n != fi.Size() {
			return errors.Errorf("unexpected nr of bytes written to tar, saw '%d' on-disk but only wrote '%d', is directory '%s' in use?", fi.Size(), n, dir)
		}

		return nil
	})

	if err != nil {
		return errors.Wrapf(err, "failed to walk dir '%s'", dir)
	}

	if err = tw.Close(); err != nil {
		if err == io.ErrClosedPipe {
			return err
		}
		return errors.Wrap(err, "failed to write remaining data")
	}

	return nil
}

//untardir untars an archive from the reader to a directory on disk.
func untardir(dir string, r io.Reader) (err error) {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err != nil {
			if err == io.EOF {
				break
			}

			return errors.Wrap(err, "failed to read next tar header")
		}

		path := safeFilePath(filepath.Join(dir, hdr.Name))
		err = os.MkdirAll(filepath.Dir(path), 0777)
		if err != nil {
			return errors.Wrap(err, "failed to create dirs")
		}

		f, err := safefile.Create(path, os.FileMode(hdr.Mode))
		if err != nil {
			return errors.Wrap(err, "failed to create tmp safe file")
		}

		defer f.Close()
		n, err := io.Copy(f, tr)
		if err != nil {
			return errors.Wrap(err, "failed to write file content to tmp file")
		}

		if n != hdr.Size {
			return errors.Errorf("unexpected nr of bytes written, wrote '%d' saw '%d' in tar hdr", n, hdr.Size)
		}

		err = f.Commit()
		if err != nil {
			return errors.Wrap(err, "failed to swap old file for tmp file")
		}

		err = os.Chtimes(path, time.Now(), hdr.ModTime)
		if err != nil {
			return errors.Wrap(err, "failed to change times of tmp file")
		}
	}

	return nil
}

//safeFilePath returns a unique filename for a given filepath.
//For example: file.txt will become file_(1).txt if file.txt is already present.
func safeFilePath(p string) string {
	_, err := os.Stat(p)
	if err != nil && os.IsNotExist(err) {
		return p
	}
	filename := filepath.Base(p)
	ext := filepath.Ext(filename)
	clean := strings.TrimSuffix(filename, ext)
	re := regexp.MustCompile("_\\(\\d+\\)$")
	versionMatch := re.FindString(clean)
	version := 1
	if versionMatch != "" {
		oldVersion, _ := strconv.Atoi(strings.Trim(versionMatch, "_()"))
		clean = strings.TrimSuffix(clean, fmt.Sprintf("_(%v)", oldVersion))
		version = oldVersion + 1
	}
	newFilename := fmt.Sprintf("%s_(%v)%s", clean, version, ext)
	newPath := path.Join(filepath.Dir(p), newFilename)
	return safeFilePath(newPath)
}

//countBytes counts all bytes from a reader.
func countBytes(r io.Reader) (total int64, err error) {
	buf := make([]byte, 512*1024)
	for {
		n, err := io.ReadFull(r, buf)
		if err == io.ErrUnexpectedEOF {
			err = nil
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, errors.Wrap(err, "failed to read part of tar")
		}
		total = total + int64(n)
	}
	return total, nil
}
