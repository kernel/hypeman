package instances

import "time"

type Instance struct {
	Id        string
	Name      string
	Image     string
	CreatedAt time.Time
}

type CreateInstanceRequest struct {
	Id    string
	Name  string
	Image string
}

type AttachVolumeRequest struct {
	MountPath string
}

