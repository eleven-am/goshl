package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

func JobDigest(job Job) string {
	job.Digest = ""
	job.EnqueuedAt, job.StartBy = time.Time{}, time.Time{}
	data, err := json.Marshal(job)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
