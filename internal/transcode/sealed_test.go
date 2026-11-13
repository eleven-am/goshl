package transcode

import "github.com/eleven-am/goshl/internal/domain"

func sealed(job domain.Job) domain.Job {
	job.Digest = domain.JobDigest(job)
	return job
}
