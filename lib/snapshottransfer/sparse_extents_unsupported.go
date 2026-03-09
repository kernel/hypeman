//go:build !(darwin || linux)

package snapshottransfer

type fileDataExtent struct {
	FileOffset int64
	Length     int64
}

func listFileDataExtents(path string) ([]fileDataExtent, error) {
	_ = path
	return nil, nil
}
