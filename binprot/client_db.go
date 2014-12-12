// Binary protocol over IPC - DB management features (client).

package binprot

import (
	"fmt"
	"github.com/HouzuoGuo/tiedot/db"
	"github.com/HouzuoGuo/tiedot/tdlog"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

func (client *BinProtClient) forAllDBsDo(fun func(*db.DB) error) error {
	for i := 0; i < client.nProcs; i++ {
		clientDB, err := db.OpenDB(path.Join(client.workspace, strconv.Itoa(i)))
		if err != nil {
			return err
		} else if err = fun(clientDB); err != nil {
			return err
		} else if err = clientDB.Close(); err != nil {
			return err
		}
	}
	return nil
}

// Create a new collection.
func (client *BinProtClient) Create(colName string) error {
	return client.reqMaintAccess(func() error {
		return client.forAllDBsDo(func(clientDB *db.DB) error {
			return clientDB.Create(colName)
		})
	})
}

// Return all collection names sorted in alphabetical order.
func (client *BinProtClient) AllCols() (names []string) {
	if err := client.Ping(); err != nil {
		tdlog.Noticef("Client %d: failed to ping before returning collection names - %v", client.id, err)
	}
	client.opLock.Lock()
	names = make([]string, 0, len(client.schema.colNameLookup))
	for name := range client.schema.colNameLookup {
		names = append(names, name)
	}
	client.opLock.Unlock()
	sort.Strings(names)
	return
}

// Rename a collection.
func (client *BinProtClient) Rename(oldName, newName string) error {
	return client.reqMaintAccess(func() error {
		return client.forAllDBsDo(func(clientDB *db.DB) error {
			return clientDB.Rename(oldName, newName)
		})
	})
}

// Truncate a collection
func (client *BinProtClient) Truncate(colName string) error {
	return client.reqMaintAccess(func() error {
		return client.forAllDBsDo(func(clientDB *db.DB) error {
			return clientDB.Truncate(colName)
		})
	})
}

// Compact a collection and recover corrupted documents.
func (client *BinProtClient) Scrub(colName string) error {
	return client.reqMaintAccess(func() error {
		// Remember existing indexes
		existingIndexes := make([][]string, 0, 0)
		colID, exists := client.schema.colNameLookup[colName]
		if !exists {
			return fmt.Errorf("Collection does not exist")
		}
		for _, existingIndex := range client.schema.indexPaths[colID] {
			existingIndexes = append(existingIndexes, existingIndex)
		}
		// Create a temporary collection for holding good&clean documents
		tmpColName := fmt.Sprintf("scrub-%s-%d", colName, time.Now().UnixNano())
		err := client.forAllDBsDo(func(clientDB *db.DB) error {
			if err := clientDB.Create(tmpColName); err != nil {
				return err
			}
			// Recreate all indexes
			for _, existingIndex := range existingIndexes {
				if err := clientDB.Use(tmpColName).BPIndex(existingIndex); err != nil {
					return err
				}
			}
			return nil
		})
		// Reload schema so that servers & client know the temp collection
		if err != nil {
			return err
		} else if err = client.reloadServer(); err != nil {
			return err
		}
		tmpColID, exists := client.schema.colNameLookup[tmpColName]
		if !exists {
			return fmt.Errorf("TmpCol went missing?!")
		}
		// Put documents back in - 10k at a time
		docCount, err := client.approxDocCount(colName)
		if err != nil {
			return err
		}
		total := docCount/10000 + 1
		for page := uint64(0); page < total; page++ {
			docs, err := client.getDocPage(colName, page, total, true)
			if err != nil {
				return err
			}
			for docID, doc := range docs {
				if err := client.insertRecovery(tmpColID, docID, doc); err != nil {
					return err
				}
			}
		}
		// Replace the original collection by the good&clean one
		err = client.forAllDBsDo(func(clientDB *db.DB) error {
			if err := clientDB.Drop(colName); err != nil {
				return err
			} else if err := clientDB.Rename(tmpColName, colName); err != nil {
				return err
			}
			return nil
		})
		if err != nil {
			return err
		} else if err = client.reloadServer(); err != nil {
			return err
		}
		return nil
	})
}

// Drop a collection.
func (client *BinProtClient) Drop(colName string) error {
	return client.reqMaintAccess(func() error {
		for i := 0; i < client.nProcs; i++ {
			if clientDB, err := db.OpenDB(path.Join(client.workspace, strconv.Itoa(i))); err != nil {
				return err
			} else if err = clientDB.Drop(colName); err != nil {
				return err
			} else if err = clientDB.Close(); err != nil {
				return err
			}
		}
		return nil
	})
}

// Copy database into destination directory (for backup).
func (client *BinProtClient) DumpDB(destDir string) error {
	return client.reqMaintAccess(func() error {
		for i := 0; i < client.nProcs; i++ {
			destDirPerRank := path.Join(destDir, strconv.Itoa(i))
			if err := os.MkdirAll(destDirPerRank, 0700); err != nil {
				return err
			} else if clientDB, err := db.OpenDB(path.Join(client.workspace, strconv.Itoa(i))); err != nil {
				return err
			} else if err = clientDB.Dump(destDirPerRank); err != nil {
				return err
			} else if err = clientDB.Close(); err != nil {
				return err
			}
		}
		return nil
	})
}

// Create an index.
func (client *BinProtClient) Index(colName string, idxPath []string) error {
	return client.reqMaintAccess(func() error {
		for i := 0; i < client.nProcs; i++ {
			if clientDB, err := db.OpenDB(path.Join(client.workspace, strconv.Itoa(i))); err != nil {
				return err
			} else if clientDB.Use(colName) == nil {
				return fmt.Errorf("Collection does not exist")
			} else if err = clientDB.Use(colName).BPIndex(idxPath); err != nil {
				return err
			} else if err = clientDB.Close(); err != nil {
				return err
			}
		}
		// Refresh schema on server and myself
		if err := client.reloadServer(); err != nil {
			return err
		}
		// Figure out the hash table ID
		newHTID := client.schema.GetHTIDByPath(colName, idxPath)
		if newHTID == -1 {
			return fmt.Errorf("New hash table is missing?!")
		}
		htIDBytes := Bint32(newHTID)
		// Reindex documents - 10k at a time
		docCount, err := client.approxDocCount(colName)
		if err != nil {
			return err
		}
		total := docCount/10000 + 1
		for page := uint64(0); page < total; page++ {
			docs, err := client.getDocPage(colName, page, total, true)
			if err != nil {
				return err
			}
			// A simplified client.indexDoc
			for docID, doc := range docs {
				docIDBytes := Buint64(docID)
				for _, val := range db.GetIn(doc, idxPath) {
					if val != nil {
						htKey := db.StrHash(fmt.Sprint(val))
						if _, _, err := client.sendCmd(int(htKey%uint64(client.nProcs)), false, C_HT_PUT, htIDBytes, Buint64(htKey), docIDBytes); err != nil {
							return err
						}
					}
				}
			}
		}
		return nil
	})
}

// Return all indexed paths in a collection. Results are sorted in alphabetical order.
func (client *BinProtClient) AllIndexes(colName string) (paths [][]string, err error) {
	jointPath, err := client.AllIndexesJointPaths(colName)
	if err != nil {
		return
	}
	paths = make([][]string, len(jointPath))
	for i, aPath := range jointPath {
		paths[i] = strings.Split(aPath, db.INDEX_PATH_SEP)
	}
	return
}

// Return all indexed paths. Index path segments are joint together, and results are sorted in alphabetical order.
func (client *BinProtClient) AllIndexesJointPaths(colName string) (paths []string, err error) {
	paths = make([]string, 0, 0)
	if err := client.Ping(); err != nil {
		tdlog.Noticef("Client %d: failed to ping before returning index paths - %v", client.id, err)
	}
	client.opLock.Lock()
	colID, exists := client.schema.colNameLookup[colName]
	if !exists {
		client.opLock.Unlock()
		return nil, fmt.Errorf("Collection %s does not exist", colName)
	}
	// Join and sort
	for path := range client.schema.indexPathsJoint[colID] {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	client.opLock.Unlock()
	return
}

// Remove an index.
func (client *BinProtClient) Unindex(colName string, idxPath []string) error {
	return client.reqMaintAccess(func() error {
		for i := 0; i < client.nProcs; i++ {
			if clientDB, err := db.OpenDB(path.Join(client.workspace, strconv.Itoa(i))); err != nil {
				return err
			} else if clientDB.Use(colName) == nil {
				continue
			} else if err = clientDB.Use(colName).Unindex(idxPath); err != nil {
				return err
			} else if err = clientDB.Close(); err != nil {
				return err
			}
		}
		return nil
	})
}
