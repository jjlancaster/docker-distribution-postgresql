package pgdriver

import (
	"bytes"
	"database/sql"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/docker/distribution/context"
	"github.com/docker/distribution/registry/auth"
	storagedriver "github.com/docker/distribution/registry/storage/driver"
	"github.com/docker/distribution/registry/storage/driver/base"
	"github.com/docker/distribution/registry/storage/driver/factory"

	"github.com/mitchellh/mapstructure"
	"github.com/noxiouz/go-postgresql-cluster/pgcluster"
	"github.com/pborman/uuid"

	// PostgreSQL backend for database/sql
	_ "github.com/lib/pq"
)

const (
	driverSQLName = "postgres"
	driverName    = "postgres"

	tableMeta = "mfs"
)

const (
	// checks if the file or dir exists and returns its type
	checksFileExistsAndGetType = "SELECT dir FROM mfs WHERE path=$1"
	// inserts metainformation about file or dir
	insertMetaAboutFileOrDir = "INSERT INTO mfs (path, parent, dir, size, modtime, key, owner) VALUES ($1, $2, $3, $4, now(), $5, $6)"
)

func init() {
	factory.Register(driverName, &factoryPostgreDriver{})
}

func generateKey() string {
	return uuid.NewRandom().String()
}

func isRoot(path string) bool {
	return path == "/"
}

type postgreDriverConfig struct {
	URLs           []string
	ConnectTimeout time.Duration
	MaxOpenConns   int
	// pointer is here to distinguish 0 vlaue from zerovalue by comparing with `nil`
	MaxIdleConns *int

	Type    string
	Options map[string]interface{}
}

type factoryPostgreDriver struct{}

func decodeConfig(parameters map[string]interface{}, config interface{}) error {
	decoder, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		WeaklyTypedInput: true,
		Result:           config,
	})

	if err != nil {
		return err
	}

	return decoder.Decode(parameters)
}

func (f *factoryPostgreDriver) Create(parameters map[string]interface{}) (storagedriver.StorageDriver, error) {
	var (
		config postgreDriverConfig
	)

	if err := decodeConfig(parameters, &config); err != nil {
		return nil, err
	}

	return pgdriverNew(&config)
}

type driver struct {
	cluster *pgcluster.Cluster
	storage KVStorage
}

type baseEmbed struct {
	base.Base
}

// Driver implements Storage interface. It uses PostgreSQL and plain KV storage to save data
type Driver struct {
	baseEmbed
}

func pgdriverNew(cfg *postgreDriverConfig) (*Driver, error) {
	var (
		st  KVStorage
		err error
	)

	cluster, err := pgcluster.NewPostgreSQLCluster(driverSQLName, cfg.URLs)
	if err != nil {
		return nil, err
	}

	if err = cluster.DB(pgcluster.MASTER).Ping(); err != nil {
		return nil, err
	}

	if cfg.MaxOpenConns != 0 {
		cluster.SetMaxOpenConns(cfg.MaxOpenConns)
	}

	if cfg.MaxIdleConns != nil {
		cluster.SetMaxIdleConns(*cfg.MaxIdleConns)
	}

	switch cfg.Type {
	case "inmemory":
		st, err = newInMemory()
	case "mds":
		st, err = newMDSBinStorage(cluster, cfg.Options)
	default:
		cluster.Close()
		return nil, fmt.Errorf("Unsupported binary storage backend %s", cfg.Type)
	}

	if err != nil {
		cluster.Close()
		return nil, err
	}

	d := &Driver{
		baseEmbed: baseEmbed{
			Base: base.Base{
				StorageDriver: &driver{
					cluster: cluster,
					storage: st,
				},
			},
		},
	}
	return d, nil
}

// Name returns the driver name
func (d *driver) Name() string {
	return driverName
}

// GetContent retrieves the content stored at "path" as a []byte.
// This should primarily be used for small objects.
func (d *driver) GetContent(ctx context.Context, path string) ([]byte, error) {
	key, err := d.getKey(ctx, d.cluster.DB(pgcluster.MASTER), path)
	if err != nil {
		return nil, err
	}

	reader, err := d.storage.Get(ctx, key, 0)
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	var output = new(bytes.Buffer)
	if _, err := io.Copy(output, reader); err != nil {
		return nil, err
	}

	return output.Bytes(), nil
}

type rowQuerier interface {
	QueryRow(query string, args ...interface{}) *sql.Row
}

func (d *driver) getKey(ctx context.Context, db rowQuerier, path string) (string, error) {
	var key string
	err := db.QueryRow("SELECT key FROM mfs WHERE path=$1", path).Scan(&key)
	switch err {
	case sql.ErrNoRows:
		return "", storagedriver.PathNotFoundError{Path: path, DriverName: driverName}
	case nil:
		return key, nil
	default:
		return "", err
	}
}

// PutContent stores the []byte content at a location designated by "path".
// This should primarily be used for small objects.
func (d *driver) PutContent(ctx context.Context, path string, content []byte) error {
	writer, err := d.Writer(ctx, path, false)
	if err != nil {
		return err
	}
	defer writer.Close()
	_, err = io.Copy(writer, bytes.NewReader(content))
	if err != nil {
		writer.Cancel()
		return err
	}
	return writer.Commit()
}

// Writer returns a FileWriter which will store the content written to it
// at the location designated by "path" after the call to Commit.
func (d *driver) Writer(ctx context.Context, path string, append bool) (storagedriver.FileWriter, error) {
	return newFileWriter(ctx, d, path, append)
}

// Reader retrieves an io.ReadCloser for the content stored at "path"
// with a given byte offset.
// May be used to resume reading a stream by providing a nonzero offset.
func (d *driver) Reader(ctx context.Context, path string, offset int64) (io.ReadCloser, error) {
	key, err := d.getKey(ctx, d.cluster.DB(pgcluster.MASTER), path)
	if err != nil {
		return nil, err
	}
	return d.storage.Get(ctx, key, offset)
}

// Stat retrieves the FileInfo for the given path, including the current
// size in bytes and the creation time.
func (d *driver) Stat(ctx context.Context, path string) (storagedriver.FileInfo, error) {
	info := storagedriver.FileInfoFields{
		Path: path,
	}

	// NOTE: should size of directory be evaluated as total size of its childs?
	err := d.cluster.DB(pgcluster.MASTER).QueryRow("SELECT dir, size, modtime FROM mfs WHERE path=$1", path).Scan(&info.IsDir, &info.Size, &info.ModTime)
	switch err {
	case sql.ErrNoRows:
		return nil, storagedriver.PathNotFoundError{Path: path, DriverName: driverName}
	case nil:
		return &storagedriver.FileInfoInternal{FileInfoFields: info}, nil
	default:
		return nil, err
	}
}

// List returns a list of the objects that are direct descendants of the given path.
func (d *driver) List(ctx context.Context, path string) ([]string, error) {
	//NOTE: should I use Tx?
	if !isRoot(path) {
		var ph interface{}
		switch err := d.cluster.DB(pgcluster.MASTER).QueryRow("SELECT 1 FROM mfs WHERE path=$1", path).Scan(&ph); err {
		case sql.ErrNoRows:
			return nil, storagedriver.PathNotFoundError{Path: path, DriverName: driverName}
		case nil:
			// pass
		default:
			return nil, err
		}
	}

	rows, err := d.cluster.DB(pgcluster.MASTER).Query("SELECT path FROM mfs WHERE parent=$1", path)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var listing []string
	for rows.Next() {
		var item string
		if err := rows.Scan(&item); err != nil {
			return nil, err
		}
		listing = append(listing, item)
	}
	return listing, nil
}

// Move moves an object stored at sourcePath to destPath, removing the
// original object.
func (d *driver) Move(ctx context.Context, sourcePath string, destPath string) error {
	tx, err := d.cluster.DB(pgcluster.MASTER).Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Check that the source exists and is a file.
	var isDir = false
	switch err := tx.QueryRow(checksFileExistsAndGetType, sourcePath).Scan(&isDir); err {
	case sql.ErrNoRows:
		return storagedriver.PathNotFoundError{Path: sourcePath}
	case nil:
		if isDir {
			return fmt.Errorf("source `%s` is a directory. Moving directories is not supported", sourcePath)
		}
	default:
		return err
	}

	var owner = ctx.Value(auth.UserNameKey)

	// Check that the dest is not a directory.
	switch err := tx.QueryRow(checksFileExistsAndGetType, destPath).Scan(&isDir); err {
	case sql.ErrNoRows:
		parent := filepath.Dir(destPath)
		var (
			size int64
			key  sql.NullString
		)

		if err = tx.QueryRow(`DELETE FROM mfs WHERE path = $1 RETURNING size, key`, sourcePath).Scan(&size, &key); err != nil {
			return err
		}

		_, err = tx.Exec(`INSERT INTO mfs (path, parent, dir, size, modtime, key, owner) VALUES ($1, $2, false, $3, now(), $4, $5)`, destPath, parent, size, key, owner)
		if err != nil {
			return err
		}

	DIRECTORY_CREATION_LOOP:
		for dir, filename := filepath.Dir(parent), filepath.Base(parent); !isRoot(filename) && filename != "."; dir, filename = filepath.Dir(dir), filepath.Base(dir) {
			var (
				fullpath = filepath.Join(dir, filename)
				isDir    = false
			)

			switch err = tx.QueryRow(checksFileExistsAndGetType, fullpath).Scan(&isDir); err {
			case nil:
				if !isDir {
					return fmt.Errorf("unable to rewrite file by directory: %s", destPath)
				}
				break DIRECTORY_CREATION_LOOP
			case sql.ErrNoRows:
				// pass
			default:
				return err
			}

			_, err = tx.Exec(insertMetaAboutFileOrDir, fullpath, dir, true, 0, nil, owner)
			if err != nil {
				return err
			}
		}

	case nil:
		if isDir {
			return fmt.Errorf("destination `%s` is a directory. Moving directories is not supported", destPath)
		}
		// TODO: looks ugly. Actually I can merge previous queries here by adding dir = true
		// Delete source record and update dest record with some fields
		_, err = tx.Exec(`
			WITH t AS (DELETE FROM mfs WHERE path = $1 RETURNING size, key)
			UPDATE mfs SET (size, modtime, key) = (t.size, now(), t.key)
			FROM t WHERE mfs.path = $2;`, sourcePath, destPath)
		if err != nil {
			return err
		}
	default:
		return err
	}

	return tx.Commit()
}

// Delete recursively deletes all objects stored at "path" and its subpaths.
func (d *driver) Delete(ctx context.Context, path string) error {
	tx, err := d.cluster.DB(pgcluster.MASTER).Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var (
		// NOTE: intended to be used to mark files in MDS table
		deleted []string

		key   sql.NullString
		isDir = false
	)

	if !isRoot(path) {
		err = tx.QueryRow("DELETE FROM mfs WHERE mfs.path = $1 RETURNING mfs.key, mfs.dir", path).Scan(&key, &isDir)
		switch err {
		case nil:
			if key.Valid {
				deleted = append(deleted, key.String)
			}
		case sql.ErrNoRows:
			return storagedriver.PathNotFoundError{Path: path}
		default:
			return err
		}
	}

	// NOTE: scan for childs only if a directory is being deleted
	if isDir {
		// TODO: it's possible to add optimization for dir only RECURSIVE scanning
		rows, err := tx.Query(`
			WITH RECURSIVE t(path) AS (
			        SELECT path FROM mfs WHERE parent = $1
			    UNION ALL
			        SELECT mfs.path FROM t, mfs WHERE mfs.parent = t.path
			)
			DELETE FROM mfs USING t WHERE mfs.path = t.path RETURNING mfs.key;
		`, path)
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			if err := rows.Scan(&key); err != nil {
				return err
			}

			if key.Valid {
				deleted = append(deleted, key.String)
			}
		}
	}

	for _, key := range deleted {
		if err := d.storage.Delete(ctx, key); err != nil {
			context.GetLoggerWithFields(ctx, map[interface{}]interface{}{"key": key, "error": err.Error()}).Error("KVStorage.Delete")
		}
	}

	// TODO: mark fields in MDS table before commit from `deleted` array
	return tx.Commit()
}

// URLFor returns a URL which may be used to retrieve the content stored at
// the given path, possibly using the given options.
func (d *driver) URLFor(ctx context.Context, path string, options map[string]interface{}) (string, error) {
	key, err := d.getKey(ctx, d.cluster.DB(pgcluster.MASTER), path)
	if err != nil {
		return "", err
	}

	return d.storage.URLFor(ctx, key)
}

// fileWriter provides an abstraction for an opened writable file-like object in
// the storage backend. The FileWriter must flush all content written to it on
// the call to Close, but is only required to make its content readable on a
// call to Commit.
type fileWriter struct {
	context.Context
	*driver

	buff   *bytes.Buffer
	path   string
	key    string
	append bool

	size int64

	closed    bool
	committed bool
	cancelled bool
}

func newFileWriter(ctx context.Context, driver *driver, path string, append bool) (storagedriver.FileWriter, error) {
	fw := &fileWriter{
		Context: ctx,
		driver:  driver,

		buff:   new(bytes.Buffer),
		path:   path,
		append: append,
	}

	if append {
		var key sql.NullString

		err := fw.driver.cluster.DB(pgcluster.MASTER).QueryRow("SELECT size, key FROM mfs WHERE path=$1", path).Scan(&fw.size, &key)
		switch err {
		case sql.ErrNoRows:
			fw.size = 0
			fw.key = generateKey()
		case nil:
			if !key.Valid {
				return nil, fmt.Errorf("Trying to append to a directory file: %s", path)
			}
			fw.key = key.String
		default:
			return nil, err
		}
	} else {
		fw.key = generateKey()
	}

	return fw, nil
}

func (fw *fileWriter) Write(p []byte) (int, error) {
	if fw.closed {
		return 0, fmt.Errorf("already closed")
	} else if fw.committed {
		return 0, fmt.Errorf("already committed")
	} else if fw.cancelled {
		return 0, fmt.Errorf("already cancelled")
	}

	nn, err := fw.buff.Write(p)
	fw.size += int64(nn)
	if err != nil {
		return nn, err
	}

	return nn, nil
}

func (fw *fileWriter) Close() error {
	if fw.closed {
		return fmt.Errorf("already closed")
	}

	fw.closed = true

	if !fw.committed {
		if fw.append {
			return fw.appendData()
		}

		return fw.storeData()
	}

	return nil
}

// Size returns the number of bytes written to this FileWriter.
func (fw *fileWriter) Size() int64 {
	return fw.size
}

// Cancel removes any written content from this FileWriter.
func (fw *fileWriter) Cancel() error {
	if fw.closed {
		return fmt.Errorf("already closed")
	}
	fw.cancelled = true

	return nil
}

func (fw *fileWriter) appendData() error {
	_, err := fw.driver.storage.Append(fw.Context, fw.key, fw.buff)
	switch err {
	case nil:
		_, err = fw.driver.cluster.DB(pgcluster.MASTER).Exec("UPDATE mfs SET size = $1 WHERE (path = $2)", fw.size, fw.path)
		if err != nil {
			return err
		}
		return nil
	case ErrAppendUnsupported:
		return &storagedriver.Error{
			DriverName: driverName,
			Enclosed:   fmt.Errorf("BinaryStorage does not support append"),
		}
	default:
		return err
	}
}

func (fw *fileWriter) storeData() error {
	if _, err := fw.driver.storage.Store(fw.Context, fw.key, fw.buff); err != nil {
		return err
	}

	var owner = fw.Context.Value(auth.UserNameKey)
	tx, err := fw.driver.cluster.DB(pgcluster.MASTER).Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Check and insert file
	var isDir = false
	switch err = tx.QueryRow(checksFileExistsAndGetType, fw.path).Scan(&isDir); err {
	case nil:
		if isDir {
			return fmt.Errorf("unable to rewrite directory by file: %s", fw.path)
		}
		if _, err = tx.Exec("DELETE FROM mfs WHERE path=$1", fw.path); err != nil {
			return err
		}
	case sql.ErrNoRows:
		// pass
	default:
		return err
	}

	// NOTE: may be update would be useful
	// NOTE: calculate size properly
	if _, err = tx.Exec(insertMetaAboutFileOrDir, fw.path, filepath.Dir(fw.path), false, fw.size, fw.key, owner); err != nil {
		return err
	}

	// TODO: wrap into a function
	parent := filepath.Dir(fw.path)
DIRECTORY_CREATION_LOOP:
	for dir, filename := filepath.Dir(parent), filepath.Base(parent); filename != "/" && filename != "."; dir, filename = filepath.Dir(dir), filepath.Base(dir) {
		var (
			fullpath = filepath.Join(dir, filename)
			isDir    = false
		)

		switch err = tx.QueryRow(checksFileExistsAndGetType, fullpath).Scan(&isDir); err {
		case nil:
			if !isDir {
				return fmt.Errorf("unable to rewrite file by directory: %s", fw.path)
			}
			break DIRECTORY_CREATION_LOOP
		case sql.ErrNoRows:
			// pass
		default:
			return err
		}

		_, err = tx.Exec(insertMetaAboutFileOrDir, fullpath, dir, true, 0, nil, owner)
		if err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	return nil
}

// Commit flushes all content written to this FileWriter and makes it
// available for future calls to StorageDriver.GetContent and
// StorageDriver.Reader.
func (fw *fileWriter) Commit() error {
	if fw.closed {
		return fmt.Errorf("already closed")
	} else if fw.committed {
		return fmt.Errorf("already committed")
	} else if fw.cancelled {
		return fmt.Errorf("already cancelled")
	}

	// NOTE: right now writting with offset is not supported by MDS, so there's no point to implemnet it now.
	// It could be added to testing backend, but it should UPDATE if key already exist and insert if it does not.
	if fw.append {
		return fw.appendData()
	}

	if err := fw.storeData(); err != nil {
		return err
	}

	fw.committed = true
	return nil
}
