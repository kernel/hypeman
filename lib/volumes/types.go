package volumes

import "time"

type Volume struct {
	Id        string
	Name      string
	SizeGb    int
	CreatedAt time.Time
}

type CreateVolumeRequest struct {
	Name   string
	SizeGb int
	Id     *string
}

