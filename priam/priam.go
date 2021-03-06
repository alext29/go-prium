package priam

import (
	"fmt"
	"github.com/golang/glog"
	"github.com/pkg/errors"
	"path"
	"strings"
	"time"
)

// Priam object provides backup and restore of cassandra DB to AWS S3.
type Priam struct {
	agent     *Agent
	cassandra *Cassandra
	config    *Config
	s3        *S3
	hist      *SnapshotHistory
}

// New returns a new Priam object.
func New(config *Config) *Priam {
	agent := NewAgent(config)
	return &Priam{
		agent:     agent,
		config:    config,
		cassandra: NewCassandra(config, agent),
		s3:        NewS3(config, agent),
	}
}

// History prints the current list of backups in S3.
func (p *Priam) History() error {

	// get snapshot history
	if err := p.SnapshotHistory(); err != nil {
		return errors.Wrap(err, "error getting snapshot history")
	}
	fmt.Printf("backup list:\n%s", p.hist)
	return nil
}

// Backup flushes all cassandra tables to disk identifies the appropriate
// files and copies them to the specified AWS S3 bucket.
func (p *Priam) Backup() error {

	glog.Infof("start taking backup...")

	// get all cassandra hosts
	hosts := p.cassandra.Hosts()
	if len(hosts) == 0 {
		return fmt.Errorf("unable to get any cassandra hosts")
	}

	// get snapshot history
	if err := p.SnapshotHistory(); err != nil {
		return errors.Wrap(err, "error getting snapshot history")
	}

	// generate new timestamp
	timestamp := p.NewTimestamp()
	glog.Infof("generating snapshot with timestamp: %s", timestamp)

	// get parent timestamp
	parent := timestamp
	snapshots := p.hist.List()

	// check timestamps are monotonically increasing
	if len(snapshots) > 0 && snapshots[len(snapshots)-1] > timestamp {
		return fmt.Errorf("new timestamp %s less than last", timestamp)
	}

	// assign parent timestamp if incremental
	if len(snapshots) > 0 && p.config.Incremental {
		parent = snapshots[len(snapshots)-1]
	} else {
		p.config.Incremental = false
	}
	glog.Infof("timestamp of parent snapshot: %s", parent)

	// perform schema backup
	if err := p.schemaBackup(parent, timestamp, hosts[0]); err != nil {
		return errors.Wrap(err, "schema backup failed")
	}

	// take snapshot on each host
	// TODO: this could be done in parallel
	for _, host := range hosts {
		glog.Infof("snapshot @ %s", host)

		// create snapshot
		files, dirs, err := p.cassandra.Snapshot(host, timestamp)
		if err != nil {
			return errors.Wrapf(err, "snapshot @ %s", host)
		}

		// upload files to s3
		if err = p.s3.UploadFiles(parent, timestamp, host, files); err != nil {
			return errors.Wrapf(err, "upload @ %s", host)
		}

		// delete local files
		if err = p.cassandra.deleteSnapshot(host, dirs); err != nil {
			return errors.Wrapf(err, "delete @ %s", host)
		}
	}
	return nil
}

func (p *Priam) schemaBackup(parent, timestamp, host string) error {

	// get schema backup
	schemaFile, err := p.cassandra.SchemaBackup(host)
	if err != nil {
		return errors.Wrap(err, "schema backup")
	}
	key := fmt.Sprintf("/%s/%s/%s/%s/%s.schema.gz",
		p.config.AwsBasePath, p.config.Keyspace,
		parent, timestamp, p.config.Keyspace)

	// upload files to s3
	if err = p.s3.UploadFile(host, schemaFile, key); err != nil {
		return errors.Wrapf(err, "schema upload @ %s", host)
	}

	return nil
}

// SnapshotHistory returns snapshot history
func (p *Priam) SnapshotHistory() error {
	if p.hist != nil {
		return nil
	}
	// get snapshot history from S3 if not already present
	h, err := p.s3.SnapshotHistory()
	if err != nil {
		return errors.Wrap(err, "error getting snapshot history")
	}
	p.hist = h
	return nil
}

// NewTimestamp generates a new timestamp which is based on current time.
// The code assumes timestamps are monotonically increasing and is used by
// restore function to determine which backup is the latest as well as the
// order of incremental backups.
func (p *Priam) NewTimestamp() string {
	return time.Now().Format("2006-01-02_150405")
}

// Restore cassandra from a given snapshot.
// TODO: if restoring from a cassandra node then skip copying file to
// cassandra host.
func (p *Priam) Restore() error {

	glog.Infof("start restoring keyspace: %s", p.config.Keyspace)

	// get all cassandra hosts
	hosts := p.cassandra.Hosts()
	if len(hosts) == 0 {
		return fmt.Errorf("did not find valid cassandra hosts")
	}
	snapshot := p.config.Snapshot

	// get snapshot history
	if err := p.SnapshotHistory(); err != nil {
		return err
	}

	// determine which snapshot to restore to
	if snapshot == "" {
		snapshots := p.hist.List()
		if len(snapshots) > 0 {
			snapshot = snapshots[len(snapshots)-1]
		}
	}
	if snapshot == "" {
		return fmt.Errorf("no existing backup to restore from")
	}

	// check if this a valid snapshot
	if !p.hist.Valid(snapshot) {
		return fmt.Errorf("%s is not a valid snapshot", snapshot)
	}
	glog.Infof("restoring to snapshot: %s", snapshot)

	// drop keyspace
	if err := p.deleteKeyspace(hosts[0]); err != nil {
		return errors.Wrap(err, "error deleting keyspace")
	}

	// create schema
	if err := p.createSchema(hosts[0], snapshot); err != nil {
		return errors.Wrap(err, "error creating schema")
	}

	// load data
	if err := p.loadSnapshot(hosts[0], snapshot); err != nil {
		return errors.Wrap(err, "error loading snapshot")
	}
	return nil
}

// deleteKeyspace deletes keyspace.
func (p *Priam) deleteKeyspace(host string) error {

	cmd := fmt.Sprintf("echo 'DROP KEYSPACE IF EXISTS %s;' | %s",
		p.config.Keyspace, p.config.CqlshPath)
	_, err := p.agent.Run(host, cmd)
	if err != nil {
		return err
	}
	return nil
}

// createSchema creates the schema from backup for given snapshot.
func (p *Priam) createSchema(host, snapshot string) error {

	// get parent
	parent := p.hist.Parent(snapshot)

	// schema key
	key := fmt.Sprintf("/%s/%s/%s/%s/%s.schema.gz",
		p.config.AwsBasePath, p.config.Keyspace,
		parent, snapshot, p.config.Keyspace)

	localTmpDir := fmt.Sprintf("%s/local", p.config.TempDir)
	remoteTmpDir := fmt.Sprintf("%s/remote", p.config.TempDir)

	// download schema file
	localFile, err := p.s3.downloadKey(key, localTmpDir)
	if err != nil {
		return errors.Wrap(err, "error downloading schema key")
	}

	// copy schema file to cassandra host
	remoteFile := strings.TrimSuffix(path.Join(remoteTmpDir, key), ".gz")
	err = p.agent.UploadFile(host, localFile, path.Dir(remoteFile))
	if err != nil {
		return errors.Wrap(err, "error uploading file")
	}

	// create schema
	cmd := fmt.Sprintf("cat %s | %s", remoteFile, p.config.CqlshPath)
	_, err = p.agent.Run(host, cmd)
	if err != nil {
		return errors.Wrap(err, "failed creating schema")
	}
	return nil
}

// loadSnapshot loads snapshot to cassandra.
func (p *Priam) loadSnapshot(host, snapshot string) error {

	localTmpDir := fmt.Sprintf("%s/local", p.config.TempDir)
	remoteTmpDir := fmt.Sprintf("%s/remote", p.config.TempDir)

	// get list of keys to download
	keys, err := p.hist.Keys(snapshot)
	if err != nil {
		return errors.Wrap(err, "failed to get all keys")
	}

	// download keys
	files, err := p.s3.downloadKeys(keys, localTmpDir)
	if err != nil {
		return errors.Wrap(err, "error downloading keys")
	}

	// upload files to host
	dirs, err := p.uploadFilesToHost(host, remoteTmpDir, files)
	if err != nil {
		return errors.Wrap(err, "could not upload files to host")
	}

	// run sstableload
	err = p.cassandra.sstableload(host, dirs)
	if err != nil {
		return errors.Wrap(err, "failed to run sstableloader")
	}
	return nil
}

// uploadFilesToHost copies cassandra files to a local directory on
// one of the cassandra hosts.
func (p *Priam) uploadFilesToHost(host, remoteTmpDir string,
	files map[string]string) (map[string]bool, error) {

	dirs := make(map[string]bool)
	for key, localFile := range files {
		glog.V(2).Infof("copy to %s: %s", host, key)
		remoteDir := path.Dir(fmt.Sprintf("%s/%s", remoteTmpDir, key))
		err := p.agent.UploadFile(host, localFile, remoteDir)
		if err != nil {
			return nil, errors.Wrap(err, "error uploading backup files to host")
		}
		dirs[remoteDir] = true
	}
	return dirs, nil
}
