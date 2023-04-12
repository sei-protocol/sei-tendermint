package dbsync

import (
	"crypto/md5"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path"

	"github.com/tendermint/tendermint/config"
	dstypes "github.com/tendermint/tendermint/proto/tendermint/dbsync"
)

func Snapshot(height uint64, dbsyncConfig config.DBSyncConfig, baseConfig config.BaseConfig) error {
	src := path.Join(baseConfig.DBDir(), ApplicationDBSubdirectory)
	dst := path.Join(dbsyncConfig.SnapshotDirectory, fmt.Sprintf("%s%d", HeightSubdirectoryPrefix, height))
	os.RemoveAll(dst)
	err := os.MkdirAll(dst, os.ModePerm)
	if err != nil {
		return fmt.Errorf("error creating directory %s - %s", dst, err)
	}
	var fds []os.FileInfo
	if fds, err = ioutil.ReadDir(src); err != nil {
		return err
	}
	for _, fd := range fds {
		srcfp := path.Join(src, fd.Name())
		dstfp := path.Join(dst, fd.Name())

		var srcfd *os.File
		var dstfd *os.File
		if srcfd, err = os.Open(srcfp); err != nil {
			return err
		}

		if dstfd, err = os.Create(dstfp); err != nil {
			srcfd.Close()
			return err
		}

		if _, err = io.Copy(dstfd, srcfd); err != nil {
			srcfd.Close()
			dstfd.Close()
			return err
		}

		srcfd.Close()
		dstfd.Close()
	}

	metadata := dstypes.MetadataResponse{
		Height:      height,
		Filenames:   []string{},
		Md5Checksum: [][]byte{},
	}

	for _, fd := range fds {
		metadata.Filenames = append(metadata.Filenames, fd.Name())

		bz, err := ioutil.ReadFile(path.Join(dst, fd.Name()))
		if err != nil {
			return err
		}
		sum := md5.Sum(bz)
		metadata.Md5Checksum = append(metadata.Md5Checksum, sum[:])
	}

	metadataBz, err := metadata.Marshal()
	if err != nil {
		return err
	}

	metadataFile, err := os.Create(path.Join(dst, MetadataFilename))
	if err != nil {
		return err
	}
	defer metadataFile.Close()
	_, err = metadataFile.Write(metadataBz)
	if err != nil {
		return err
	}

	heightFile, err := os.Create(path.Join(dbsyncConfig.SnapshotDirectory, MetadataHeightFilename))
	if err != nil {
		return err
	}
	defer heightFile.Close()
	_, err = heightFile.Write([]byte(fmt.Sprintf("%d", height)))
	return err
}
