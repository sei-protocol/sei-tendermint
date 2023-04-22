package dbsync

import (
	"crypto/md5"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"sync"

	"github.com/tendermint/tendermint/config"
	dstypes "github.com/tendermint/tendermint/proto/tendermint/dbsync"
)

const WORKER_COUNT = 32

func Snapshot(height uint64, dbsyncConfig config.DBSyncConfig, baseConfig config.BaseConfig) error {
	src := path.Join(baseConfig.DBDir(), ApplicationDBSubdirectory)
	wasmSrc := path.Join(baseConfig.RootDir, WasmDirectory)
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
	wasmNames := map[string]struct{}{}
	if wasmFds, _ := ioutil.ReadDir(wasmSrc); wasmFds != nil {
		fds = append(fds, wasmFds...)
		for _, fd := range wasmFds {
			wasmNames[fd.Name()] = struct{}{}
		}
	}

	assignments := make([][]os.FileInfo, WORKER_COUNT)

	for i, fd := range fds {
		assignments[i%WORKER_COUNT] = append(assignments[i%WORKER_COUNT], fd)
	}

	metadata := dstypes.MetadataResponse{
		Height:      height,
		Filenames:   []string{},
		Md5Checksum: [][]byte{},
	}
	metadataMtx := &sync.Mutex{}

	wg := sync.WaitGroup{}
	for i := 0; i < WORKER_COUNT; i++ {
		wg.Add(1)
		assignment := assignments[i]
		go func() {
			for _, fd := range assignment {
				var srcfp, dstfp string
				if _, ok := wasmNames[fd.Name()]; ok {
					srcfp = path.Join(wasmSrc, fd.Name())
					dstfp = path.Join(dst, fd.Name()) + WasmSuffix
				} else {
					srcfp = path.Join(src, fd.Name())
					dstfp = path.Join(dst, fd.Name())
				}

				// var srcfd *os.File
				// var dstfd *os.File
				// if srcfd, err = os.Open(srcfp); err != nil {
				// 	panic(err)
				// }

				// if dstfd, err = os.Create(dstfp); err != nil {
				// 	srcfd.Close()
				// 	panic(err)
				// }

				// if _, err = io.Copy(dstfd, srcfd); err != nil {
				// 	srcfd.Close()
				// 	dstfd.Close()
				// 	panic(err)
				// }
				cmd := exec.Command("cp", srcfp, dstfp)
				_, err := cmd.Output()
				if err != nil {
					panic(err)
				}

				filename := fd.Name()
				if _, ok := wasmNames[fd.Name()]; ok {
					filename += WasmSuffix
				}

				bz, err := ioutil.ReadFile(path.Join(dst, filename))
				if err != nil {
					panic(err)
				}
				sum := md5.Sum(bz)

				metadataMtx.Lock()
				metadata.Filenames = append(metadata.Filenames, filename)
				metadata.Md5Checksum = append(metadata.Md5Checksum, sum[:])

				metadataMtx.Unlock()
				// srcfd.Close()
				// dstfd.Close()
			}
			wg.Done()
		}()
	}
	wg.Wait()

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
