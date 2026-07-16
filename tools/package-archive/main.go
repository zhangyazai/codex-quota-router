package main

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path"
	"path/filepath"
)

func main() {
	if len(os.Args) != 7 || writeArchive(os.Args[1], os.Args[2], os.Args[3:]) != nil {
		os.Exit(1)
	}
}

func writeArchive(output, root string, files []string) error {
	destination, err := os.Create(output)
	if err != nil {
		return err
	}
	gzipWriter := gzip.NewWriter(destination)
	tarWriter := tar.NewWriter(gzipWriter)

	if err := tarWriter.WriteHeader(&tar.Header{Name: root + "/", Mode: 0o755, Typeflag: tar.TypeDir}); err != nil {
		return err
	}
	for index, source := range files {
		mode := int64(0o644)
		if index < 2 {
			mode = 0o755
		}
		if err := addFile(tarWriter, source, path.Join(root, filepath.Base(source)), mode); err != nil {
			return err
		}
	}
	if err := tarWriter.Close(); err != nil {
		return err
	}
	if err := gzipWriter.Close(); err != nil {
		return err
	}
	return destination.Close()
}

func addFile(writer *tar.Writer, source, name string, mode int64) error {
	file, err := os.Open(source)
	if err != nil {
		return err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return err
	}
	if err := writer.WriteHeader(&tar.Header{
		Name:    name,
		Mode:    mode,
		Size:    info.Size(),
		ModTime: info.ModTime(),
	}); err != nil {
		return err
	}
	_, err = io.Copy(writer, file)
	return err
}
