package scanner

import (
	"container/list"
	"database/sql"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/pkg/errors"
	"github.com/viktorstrate/photoview/api/graphql/models"
	"github.com/viktorstrate/photoview/api/graphql/notification"
	"github.com/viktorstrate/photoview/api/utils"
)

func findAlbumsForUser(db *sql.DB, user *models.User, album_cache *AlbumScannerCache) ([]*models.Album, []error) {

	// Check if user directory exists on the file system
	if _, err := os.Stat(user.RootPath); err != nil {
		if os.IsNotExist(err) {
			return nil, []error{errors.Errorf("Photo directory for user '%s' does not exist '%s'\n", user.Username, user.RootPath)}
		} else {
			return nil, []error{errors.Errorf("Could not read photo directory for user '%s': %s\n", user.Username, user.RootPath)}
		}
	}

	type scanInfo struct {
		path     string
		parentId *int
	}

	scanQueue := list.New()
	scanQueue.PushBack(scanInfo{
		path:     user.RootPath,
		parentId: nil,
	})

	userAlbums := make([]*models.Album, 0)
	albumErrors := make([]error, 0)
	// newPhotos := make([]*models.Photo, 0)

	for scanQueue.Front() != nil {
		albumInfo := scanQueue.Front().Value.(scanInfo)
		scanQueue.Remove(scanQueue.Front())

		albumPath := albumInfo.path
		albumParentId := albumInfo.parentId

		// Read path
		dirContent, err := ioutil.ReadDir(albumPath)
		if err != nil {
			albumErrors = append(albumErrors, errors.Wrapf(err, "read directory (%s)", albumPath))
			continue
		}

		tx, err := db.Begin()
		if err != nil {
			albumErrors = append(albumErrors, errors.Wrap(err, "begin database transaction"))
			continue
		}

		log.Printf("Scanning directory: %s", albumPath)

		// Make album if not exists
		albumTitle := path.Base(albumPath)
		_, err = tx.Exec("INSERT IGNORE INTO album (title, parent_album, owner_id, path) VALUES (?, ?, ?, ?)", albumTitle, albumParentId, user.UserID, albumPath)
		if err != nil {
			albumErrors = append(albumErrors, errors.Wrap(err, "insert album into database"))
			tx.Rollback()
			continue
		}

		row := tx.QueryRow("SELECT * FROM album WHERE path = ?", albumPath)
		album, err := models.NewAlbumFromRow(row)
		if err != nil {
			albumErrors = append(albumErrors, errors.Wrapf(err, "get album from database (%s)", albumPath))
			tx.Rollback()
			continue
		}
		userAlbums = append(userAlbums, album)

		// Commit album transaction
		if err := tx.Commit(); err != nil {
			albumErrors = append(albumErrors, errors.Wrap(err, "commit database transaction"))
			continue
		}

		// Scan for sub-albums
		for _, item := range dirContent {
			subalbumPath := path.Join(albumPath, item.Name())

			// Skip if directory is hidden
			if path.Base(subalbumPath)[0:1] == "." {
				continue
			}

			if item.IsDir() && directoryContainsPhotos(subalbumPath, album_cache) {
				scanQueue.PushBack(scanInfo{
					path:     subalbumPath,
					parentId: &album.AlbumID,
				})
			}
		}
	}

	deleteErrors := deleteOldUserAlbums(db, userAlbums, user)
	albumErrors = append(albumErrors, deleteErrors...)

	return userAlbums, albumErrors
}

func directoryContainsPhotos(rootPath string, cache *AlbumScannerCache) bool {

	if contains_image := cache.AlbumContainsPhotos(rootPath); contains_image != nil {
		return *contains_image
	}

	scanQueue := list.New()
	scanQueue.PushBack(rootPath)

	scanned_directories := make([]string, 0)

	for scanQueue.Front() != nil {

		dirPath := scanQueue.Front().Value.(string)
		scanQueue.Remove(scanQueue.Front())

		scanned_directories = append(scanned_directories, dirPath)

		dirContent, err := ioutil.ReadDir(dirPath)
		if err != nil {
			ScannerError("Could not read directory: %s\n", err.Error())
			return false
		}

		for _, fileInfo := range dirContent {
			filePath := path.Join(dirPath, fileInfo.Name())
			if fileInfo.IsDir() {
				scanQueue.PushBack(filePath)
			} else {
				if isPathImage(filePath, cache) {
					cache.InsertAlbumPaths(dirPath, rootPath, true)
					return true
				}
			}
		}

	}

	for _, scanned_path := range scanned_directories {
		cache.InsertAlbumPath(scanned_path, false)
	}
	return false
}

func deleteOldUserAlbums(db *sql.DB, scannedAlbums []*models.Album, user *models.User) []error {
	if len(scannedAlbums) == 0 {
		return nil
	}

	albumPaths := make([]interface{}, len(scannedAlbums))
	for i, album := range scannedAlbums {
		albumPaths[i] = album.Path
	}

	// Delete old albums
	album_args := make([]interface{}, 0)
	album_args = append(album_args, user.UserID)
	album_args = append(album_args, albumPaths...)

	albums_questions := strings.Repeat("?,", len(albumPaths))[:len(albumPaths)*2-1]
	rows, err := db.Query("SELECT album_id FROM album WHERE album.owner_id = ? AND path NOT IN ("+albums_questions+")", album_args...)
	if err != nil {
		return []error{errors.Wrap(err, "get albums to be deleted from database")}
	}
	defer rows.Close()

	deleteErrors := make([]error, 0)

	deleted_album_ids := make([]interface{}, 0)
	for rows.Next() {
		var album_id int
		if err := rows.Scan(&album_id); err != nil {
			deleteErrors = append(deleteErrors, errors.Wrapf(err, "parse album to be removed (album_id %d)", album_id))
			continue
		}

		deleted_album_ids = append(deleted_album_ids, album_id)
		cache_path := path.Join("./photo_cache", strconv.Itoa(album_id))
		err := os.RemoveAll(cache_path)
		if err != nil {
			deleteErrors = append(deleteErrors, errors.Wrapf(err, "delete unused cache folder (%s)", cache_path))
		}
	}

	if len(deleted_album_ids) > 0 {
		albums_questions = strings.Repeat("?,", len(deleted_album_ids))[:len(deleted_album_ids)*2-1]

		if _, err := db.Exec("DELETE FROM album WHERE album_id IN ("+albums_questions+")", deleted_album_ids...); err != nil {
			ScannerError("Could not delete old albums from database:\n%s\n", err)
			deleteErrors = append(deleteErrors, errors.Wrap(err, "delete old albums from database"))
		}
	}

	return deleteErrors
}

// func cleanupCache(database *sql.DB, cache *ScannerCache, user *models.User) {

// 	// Delete old photos
// 	photo_args := make([]interface{}, 0)
// 	photo_args = append(photo_args, user.UserID)
// 	photo_args = append(photo_args, cache.photo_paths_scanned...)

// 	photo_questions := strings.Repeat("?,", len(cache.photo_paths_scanned))[:len(cache.photo_paths_scanned)*2-1]

// 	rows, err = database.Query(`
// 		SELECT photo.photo_id as photo_id, album.album_id as album_id FROM photo JOIN album ON photo.album_id = album.album_id
// 		WHERE album.owner_id = ? AND photo.path NOT IN (`+photo_questions+`)
// 	`, photo_args...)
// 	if err != nil {
// 		ScannerError("Could not get deleted photos from database: %s\n", err)
// 		return
// 	}
// 	defer rows.Close()

// 	deleted_photo_ids := make([]interface{}, 0)

// 	for rows.Next() {
// 		var photo_id int
// 		var album_id int

// 		if err := rows.Scan(&photo_id, &album_id); err != nil {
// 			ScannerError("Could not parse photo to be removed (album_id %d, photo_id %d): %s\n", album_id, photo_id, err)
// 		}

// 		deleted_photo_ids = append(deleted_photo_ids, photo_id)
// 		cache_path := path.Join("./photo_cache", strconv.Itoa(album_id), strconv.Itoa(photo_id))
// 		err := os.RemoveAll(cache_path)
// 		if err != nil {
// 			ScannerError("Could not delete unused cache photo folder: %s\n%s\n", cache_path, err)
// 		}
// 	}

// 	if len(deleted_photo_ids) > 0 {
// 		photo_questions = strings.Repeat("?,", len(deleted_photo_ids))[:len(deleted_photo_ids)*2-1]

// 		if _, err := database.Exec("DELETE FROM photo WHERE photo_id IN ("+photo_questions+")", deleted_photo_ids...); err != nil {
// 			ScannerError("Could not delete old photos from database:\n%s\n", err)
// 		}
// 	}

// 	if len(deleted_album_ids) > 0 || len(deleted_photo_ids) > 0 {
// 		timeout := 3000
// 		notification.BroadcastNotification(&models.Notification{
// 			Key:     utils.GenerateToken(),
// 			Type:    models.NotificationTypeMessage,
// 			Header:  "Deleted old photos",
// 			Content: fmt.Sprintf("Deleted %d albums and %d photos, that was not found on disk", len(deleted_album_ids), len(deleted_photo_ids)),
// 			Timeout: &timeout,
// 		})
// 	}

// }

func ScannerError(format string, args ...interface{}) {
	message := fmt.Sprintf(format, args...)

	log.Printf("ERROR: %s", message)
	notification.BroadcastNotification(&models.Notification{
		Key:      utils.GenerateToken(),
		Type:     models.NotificationTypeMessage,
		Header:   "Scanner error",
		Content:  message,
		Negative: true,
	})
}

func PhotoCache() string {
	photoCache := os.Getenv("PHOTO_CACHE")
	if photoCache == "" {
		photoCache = "./photo_cache"
	}

	return photoCache
}
