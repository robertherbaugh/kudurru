// Package iammigrate performs the explicitly offline Kubernetes IAM cutover.
// It never starts informers, reads credential values, changes original identity
// Secrets, or authorizes requests. Root admission must remain closed until Check
// succeeds, and current authentication must recheck every returned UID binding.
package iammigrate
