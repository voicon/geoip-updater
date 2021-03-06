package maxmind

import (
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path"
	"path/filepath"

	"github.com/docker/go-units"
	"github.com/mholt/archiver/v3"
	"github.com/pkg/errors"
	"github.com/rs/zerolog/log"
)

// Downloader represents an active downloader object
type Downloader struct {
	*Client
	eid   EditionID
	dlDir string
}

// NewDownloader returns a new downloader instance
func (c *Client) NewDownloader(eid EditionID, dlDir string) (*Downloader, error) {
	var err error

	// Check download directory
	if dlDir == "" {
		dlDir = filepath.Dir(os.Args[0])
	}
	dlDir, err = filepath.Abs(path.Clean(dlDir))
	if err != nil {
		return nil, errors.Wrap(err, "Cannot get absolute path of download directory")
	}
	if err := os.MkdirAll(dlDir, 0755); err != nil {
		return nil, errors.Wrap(err, "Cannot create download directory")
	}
	if err := isDirWriteable(dlDir); err != nil {
		return nil, errors.Wrap(err, "Download directory is not writable")
	}

	return &Downloader{
		Client: c,
		eid:    eid,
		dlDir:  dlDir,
	}, nil
}

// Download downloads a database
func (d *Downloader) Download() ([]os.FileInfo, error) {
	// Retrieve expected hash
	expHash, err := d.expectedHash()
	if err != nil {
		return nil, errors.Wrap(err, "Cannot get archive MD5 hash")
	}

	// Download DB archive
	archive := path.Join(d.workDir, d.eid.Filename())
	if err := d.downloadArchive(expHash, archive); err != nil {
		return nil, err
	}

	// Create MD5 file
	md5file := path.Join(d.workDir, fmt.Sprintf(".%s.%s", d.eid.Filename(), "md5"))
	if err := createFile(md5file, expHash); err != nil {
		return nil, errors.Errorf("Cannot create MD5 file %s", md5file)
	}

	// Extract DB from archive
	dbs, err := d.extractArchive(archive)
	if err != nil {
		return nil, err
	}

	return dbs, nil
}

func (d *Downloader) expectedHash() (string, error) {
	req, err := http.NewRequest("GET", fmt.Sprintf("%s/app/geoip_download", d.baseURL), nil)
	if err != nil {
		return "", errors.Wrap(err, "Request failed")
	}

	q := req.URL.Query()
	q.Add("license_key", d.licenseKey)
	q.Add("edition_id", d.eid.String())
	q.Add("suffix", fmt.Sprintf("%s.md5", d.eid.Suffix().String()))
	req.URL.RawQuery = q.Encode()

	if d.userAgent != "" {
		req.Header.Add("User-Agent", d.userAgent)
	}

	res, err := d.http.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return "", errors.Errorf("Received invalid status code %d: %s", res.StatusCode, res.Body)
	}

	md5, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return "", errors.Wrap(err, "Cannot download MD5 file")
	}

	return string(md5), nil
}

func (d *Downloader) currentHash() (string, error) {
	md5file := path.Join(d.workDir, fmt.Sprintf(".%s.%s", d.eid.Filename(), "md5"))
	if _, err := os.Stat(md5file); os.IsNotExist(err) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	curHash, err := ioutil.ReadFile(md5file)
	if err != nil {
		return "", errors.Wrap(err, "Cannot read current archive hash")
	}
	return string(curHash), nil
}

func (d *Downloader) downloadArchive(expHash string, archive string) error {
	if _, err := os.Stat(archive); err == nil {
		curHash, err := checksumFromFile(archive)
		if err != nil {
			return errors.Wrap(err, "Cannot get archive checksum")
		}
		if expHash == curHash {
			d.log.Debug().
				Str("edition_id", d.eid.String()).
				Str("hash", expHash).
				Msgf("Archive already downloaded and valid. Skipping download")
			return nil
		}
	}

	d.log.Info().
		Str("edition_id", d.eid.String()).
		Msgf("Downloading %s archive...", filepath.Base(archive))

	req, err := http.NewRequest("GET", fmt.Sprintf("%s/app/geoip_download", d.baseURL), nil)
	if err != nil {
		return errors.Wrap(err, "Request failed")
	}

	q := req.URL.Query()
	q.Add("license_key", d.licenseKey)
	q.Add("edition_id", d.eid.String())
	q.Add("suffix", d.eid.Suffix().String())
	req.URL.RawQuery = q.Encode()

	if d.userAgent != "" {
		req.Header.Add("User-Agent", d.userAgent)
	}

	res, err := d.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return errors.Errorf("Received invalid status code %d: %s", res.StatusCode, res.Body)
	}

	out, err := os.Create(archive)
	if err != nil {
		return errors.Wrap(err, "Cannot create archive file")
	}
	defer out.Close()

	_, err = io.Copy(out, res.Body)
	if err != nil {
		return errors.Wrap(err, "Cannot download archive")
	}

	curHash, err := checksumFromFile(archive)
	if err != nil {
		return errors.Wrap(err, "Cannot get archive checksum")
	}

	if expHash != curHash {
		return errors.Errorf("MD5 of downloaded archive (%s) does not match expected md5 (%s)", curHash, expHash)
	}

	return nil
}

func (d *Downloader) extractArchive(archive string) ([]os.FileInfo, error) {
	var dbs []os.FileInfo
	err := archiver.Walk(archive, func(f archiver.File) error {
		if f.IsDir() {
			return nil
		}
		if filepath.Ext(f.Name()) != ".csv" && filepath.Ext(f.Name()) != ".mmdb" {
			return nil
		}

		expHash, reader, err := checksumFromReader(f)
		if err != nil {
			return err
		}

		sublog := log.With().
			Str("edition_id", d.eid.String()).
			Str("db_name", f.Name()).
			Str("db_size", units.HumanSize(float64(f.Size()))).
			Time("db_modtime", f.ModTime()).
			Str("db_hash", expHash).
			Logger()

		dbpath := path.Join(d.dlDir, f.Name())
		if fileExists(dbpath) {
			curHash, err := checksumFromFile(dbpath)
			if err != nil {
				return err
			}
			if expHash == curHash {
				sublog.Debug().Msg("Database is already up to date")
				return nil
			}
		}

		sublog.Debug().Msg("Extracting database")
		dbfile, err := os.Create(dbpath)
		if err != nil {
			return errors.Wrap(err, fmt.Sprintf("Cannot create database file %s", f.Name()))
		}
		defer dbfile.Close()

		_, err = io.Copy(dbfile, reader)
		if err != nil {
			return errors.Wrap(err, fmt.Sprintf("Cannot extract database file %s", f.Name()))
		}

		if err = os.Chtimes(dbpath, f.ModTime(), f.ModTime()); err != nil {
			sublog.Warn().Err(err).Msg("Cannot preserve modtime of database file")
		}

		dbs = append(dbs, f)
		return nil
	})

	return dbs, err
}
