package ns

import (
	"bfs/util/logging"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/golang/glog"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/syndtr/goleveldb/leveldb/util"
	"io"
	"strings"
	"time"
)

type FileStatus uint8

const (
	FileStatus_Unknown           FileStatus = iota
	FileStatus_UnderConstruction
	FileStatus_OK
	FileStatus_PendingDelete
)

var fileStatusStr = []string{
	"UNKNOWN",
	"UNDER_CONSTRUCTION",
	"OK",
	"PENDING_DELETE",
}

func (this *FileStatus) String() string {
	return fileStatusStr[*this]
}

type Entry struct {
	VolumeName       string
	Path             string
	Blocks           []*BlockMetadata
	Permissions      uint8
	Status           FileStatus
	BlockSize        uint64
	Size             uint64
	ReplicationLevel uint32
	Ctime            time.Time
	Mtime            time.Time
}

type BlockMetadata struct {
	Block  string
	LVName string
	PVID   string
}

type status int

const (
	namespaceStatus_INITIAL status = iota
	namespaceStatus_OPEN
	namespaceStatus_CLOSED
)

var namespaceStatusStr = []string{
	"INITIAL",
	"OPEN",
	"CLOSED",
}

func (this *status) String() string {
	return namespaceStatusStr[*this]
}

type Namespace struct {
	path string
	db   *leveldb.DB

	state status
}

type Error struct {
	error
	Path string
}

func (this *Error) Error() string {
	return ErrNoSuchEntry.Error() + " - " + this.Path
}

var ErrNoSuchEntry = errors.New("no such entry")

// Default LevelDB read and write options.
var defaultReadOpts = &opt.ReadOptions{}
var defaultWriteOpts = &opt.WriteOptions{Sync: true}

const (
	// The initial size of the result buffer for List() operations. The result
	// buffer holds pointers (*Entry) so the cost of over-allocating should be
	// small.
	listAllocSize = 1024

	dbPrefix_Entry           = byte(1)
	dbPrefix_GlobalMetadata  = byte(2)
	dbPrefix_BlockAssignment = byte(4)
)

func New(path string) *Namespace {
	return &Namespace{
		path:  path,
		state: namespaceStatus_INITIAL,
	}
}

func (this *Namespace) Open() error {
	glog.V(logging.LogLevelDebug).Infof("Opening namespace at %v", this.path)

	if this.state != namespaceStatus_INITIAL {
		return fmt.Errorf("unable to open namespace from state %v", this.state)
	}

	options := &opt.Options{
		ErrorIfMissing: false,
	}

	if db, err := leveldb.OpenFile(this.path, options); err != nil {
		return err
	} else {
		this.db = db
	}

	key := keyFor(dbPrefix_GlobalMetadata, "blockId")

	if ok, err := this.db.Has(key, defaultReadOpts); ok {
		glog.V(logging.LogLevelDebug).Info("Last blockId exists")
	} else if err != nil {
		return err
	} else {
		if err := this.db.Put(key, []byte{byte(0)}, defaultWriteOpts); err != nil {
			glog.Errorf("Failed to set initial blockId for the namespace - %v", err)
			return err
		} else {
			glog.V(logging.LogLevelDebug).Info("Initialized blockId for the namespace")
		}
	}

	this.state = namespaceStatus_OPEN

	return nil
}

func (this *Namespace) Add(entry *Entry) error {
	glog.V(logging.LogLevelDebug).Infof("Adding entry %#v", entry)

	if this.state != namespaceStatus_OPEN {
		return fmt.Errorf("unable to perform operation in state %v", this.state)
	}

	value, err := json.Marshal(entry)

	if err != nil {
		return err
	}

	key := keyFor(dbPrefix_Entry, entry.Path)

	glog.V(logging.LogLevelTrace).Infof("Serialized to entry: %v", string(value))

	if err = this.db.Put(key, value, defaultWriteOpts); err != nil {
		return err
	}

	for _, blockMetadata := range entry.Blocks {
		key := keyFor(dbPrefix_BlockAssignment, blockMetadata.Block)

		value, err := json.Marshal(blockMetadata)
		if err != nil {
			return err
		}

		glog.V(logging.LogLevelTrace).Infof("Serialized block %v", string(value))
		if err := this.db.Put(key, value, defaultWriteOpts); err != nil {
			return err
		}
	}

	return nil
}

func (this *Namespace) Get(path string) (*Entry, error) {
	glog.V(logging.LogLevelDebug).Infof("Getting entry %v", path)

	if this.state != namespaceStatus_OPEN {
		return nil, fmt.Errorf("unable to perform operation in state %v", this.state)
	}

	key := keyFor(dbPrefix_Entry, path)

	value, err := this.db.Get(key, defaultReadOpts)

	if err == leveldb.ErrNotFound {
		return nil, &Error{Path: path, error: ErrNoSuchEntry}
	} else if err != nil {
		return nil, err
	}

	var entry Entry

	if err := json.Unmarshal(value, &entry); err != nil {
		return nil, err
	}

	return &entry, nil
}

func (this *Namespace) List(from string, to string, visitor func(*Entry, error) (bool, error)) error {
	glog.V(logging.LogLevelDebug).Infof("Listing entries from %v to %v", from, to)

	if this.state != namespaceStatus_OPEN {
		return fmt.Errorf("unable to perform operation in state %v", this.state)
	}

	var startKey []byte
	if from != "" {
		startKey = keyFor(dbPrefix_Entry, from)
	}

	var endKey []byte
	if to != "" {
		endKey = keyFor(dbPrefix_Entry, to)
	}

	r := &util.Range{
		Start: startKey,
		Limit: endKey,
	}

	iter := this.db.NewIterator(r, defaultReadOpts)
	defer iter.Release()

	for iter.Next() {
		if iter.Key()[0] != dbPrefix_Entry {
			continue
		}

		var entry Entry

		if err := json.Unmarshal(iter.Value(), &entry); err != nil {
			return err
		}

		glog.V(logging.LogLevelTrace).Infof("Entry: %#v", entry)

		if ok, err := visitor(&entry, nil); err != nil {
			return err
		} else if !ok {
			break
		}
	}

	visitor(nil, io.EOF)

	return nil
}

func (this *Namespace) Delete(path string, recursive bool) (uint32, error) {
	glog.V(logging.LogLevelDebug).Infof("Deleting entry %s recursive: %t", path, recursive)

	if this.state != namespaceStatus_OPEN {
		return 0, fmt.Errorf("unable to perform operation in state %v", this.state)
	}

	batch := leveldb.Batch{}
	var entriesDeleted uint32 = 0

	this.List(path, "", func(entry *Entry, err error) (bool, error) {
		if err == io.EOF {
			return false, nil
		} else if err != nil {
			return false, err
		}

		if recursive && !strings.HasPrefix(entry.Path, path) {
			glog.V(3).Infof("recursive: %t !prefix: %t", recursive, !strings.HasPrefix(entry.Path, path))
			return false, nil
		} else if !recursive && entry.Path != path {
			glog.V(3).Infof("entry != path: %t", entry.Path != path)
			return false, nil
		}

		batch.Delete(keyFor(dbPrefix_Entry, entry.Path))
		entriesDeleted++
		return true, nil
	})

	return entriesDeleted, this.db.Write(&batch, defaultWriteOpts)
}

func (this *Namespace) Rename(from string, to string) (bool, error) {
	glog.V(logging.LogLevelDebug).Infof("Renaming entry %s to %s", from, to)

	if this.state != namespaceStatus_OPEN {
		return false, fmt.Errorf("unable to perform operation in state %v", this.state)
	}

	entry, err := this.Get(from)
	if err != nil {
		return false, err
	}

	entry.Path = to

	value, err := json.Marshal(entry)
	if err != nil {
		return false, err
	}

	batch := leveldb.Batch{}
	batch.Put(keyFor(dbPrefix_Entry, to), value)
	batch.Delete(keyFor(dbPrefix_Entry, from))

	return true, this.db.Write(&batch, defaultWriteOpts)
}

func (this *Namespace) Close() error {
	glog.V(logging.LogLevelDebug).Infof("Closing namespace at %v", this.path)

	if this.state != namespaceStatus_OPEN {
		return fmt.Errorf("unable to close namespace from state %v", this.state)
	}

	err := this.db.Close()

	this.state = namespaceStatus_CLOSED

	return err
}

func keyFor(table byte, key string) []byte {
	return bytes.Join(
		[][]byte{
			{table},
			[]byte(key),
		},
		nil,
	)
}
