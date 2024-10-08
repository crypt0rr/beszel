// Package records handles creating longer records and deleting old records.
package records

import (
	"beszel/internal/entities/container"
	"beszel/internal/entities/system"
	"log"
	"math"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/daos"
	"github.com/pocketbase/pocketbase/models"
)

type RecordManager struct {
	app *pocketbase.PocketBase
}

type LongerRecordData struct {
	shorterType            string
	longerType             string
	longerTimeDuration     time.Duration
	expectedShorterRecords int
}

type RecordDeletionData struct {
	recordType string
	retention  time.Duration
}

func NewRecordManager(app *pocketbase.PocketBase) *RecordManager {
	return &RecordManager{app}
}

// Create longer records by averaging shorter records
func (rm *RecordManager) CreateLongerRecords() {
	// start := time.Now()
	recordData := []LongerRecordData{
		{
			shorterType:            "1m",
			expectedShorterRecords: 10,
			longerType:             "10m",
			longerTimeDuration:     -10 * time.Minute,
		},
		{
			shorterType:            "10m",
			expectedShorterRecords: 2,
			longerType:             "20m",
			longerTimeDuration:     -20 * time.Minute,
		},
		{
			shorterType:            "20m",
			expectedShorterRecords: 6,
			longerType:             "120m",
			longerTimeDuration:     -120 * time.Minute,
		},
		{
			shorterType:            "120m",
			expectedShorterRecords: 4,
			longerType:             "480m",
			longerTimeDuration:     -480 * time.Minute,
		},
	}
	// wrap the operations in a transaction
	rm.app.Dao().RunInTransaction(func(txDao *daos.Dao) error {
		activeSystems, err := txDao.FindRecordsByExpr("systems", dbx.NewExp("status = 'up'"))
		if err != nil {
			log.Println("failed to get active systems", "err", err.Error())
			return err
		}

		collections := map[string]*models.Collection{}
		for _, collectionName := range []string{"system_stats", "container_stats"} {
			collection, _ := txDao.FindCollectionByNameOrId(collectionName)
			collections[collectionName] = collection
		}

		// loop through all active systems, time periods, and collections
		for _, system := range activeSystems {
			// log.Println("processing system", system.GetString("name"))
			for _, recordData := range recordData {
				// log.Println("processing longer record type", recordData.longerType)
				// add one minute padding for longer records because they are created slightly later than the job start time
				longerRecordPeriod := time.Now().UTC().Add(recordData.longerTimeDuration + time.Minute)
				// shorter records are created independently of longer records, so we shouldn't need to add padding
				shorterRecordPeriod := time.Now().UTC().Add(recordData.longerTimeDuration)
				// loop through both collections
				for _, collection := range collections {
					// check creation time of last longer record if not 10m, since 10m is created every run
					if recordData.longerType != "10m" {
						lastLongerRecord, err := txDao.FindFirstRecordByFilter(
							collection.Id,
							"type = {:type} && system = {:system} && created > {:created}",
							dbx.Params{"type": recordData.longerType, "system": system.Id, "created": longerRecordPeriod},
						)
						// continue if longer record exists
						if err == nil || lastLongerRecord != nil {
							// log.Println("longer record found. continuing")
							continue
						}
					}
					// get shorter records from the past x minutes
					allShorterRecords, err := txDao.FindRecordsByExpr(
						collection.Id,
						dbx.NewExp(
							"type = {:type} AND system = {:system} AND created > {:created}",
							dbx.Params{"type": recordData.shorterType, "system": system.Id, "created": shorterRecordPeriod},
						),
					)

					// continue if not enough shorter records
					if err != nil || len(allShorterRecords) < recordData.expectedShorterRecords {
						// log.Println("not enough shorter records. continue.", len(allShorterRecords), recordData.expectedShorterRecords)
						continue
					}
					// average the shorter records and create longer record
					var stats interface{}
					switch collection.Name {
					case "system_stats":
						stats = rm.AverageSystemStats(allShorterRecords)
					case "container_stats":
						stats = rm.AverageContainerStats(allShorterRecords)
					}
					longerRecord := models.NewRecord(collection)
					longerRecord.Set("system", system.Id)
					longerRecord.Set("stats", stats)
					longerRecord.Set("type", recordData.longerType)
					if err := txDao.SaveRecord(longerRecord); err != nil {
						log.Println("failed to save longer record", "err", err.Error())
					}
				}
			}
		}

		return nil
	})

	// log.Println("finished creating longer records", "time (ms)", time.Since(start).Milliseconds())
}

// Calculate the average stats of a list of system_stats records without reflect
func (rm *RecordManager) AverageSystemStats(records []*models.Record) system.Stats {
	var sum system.Stats
	sum.Temperatures = make(map[string]float64)
	sum.ExtraFs = make(map[string]*system.FsStats)

	count := float64(len(records))
	// use different counter for temps in case some records don't have them
	tempCount := float64(0)

	var stats system.Stats
	for _, record := range records {
		record.UnmarshalJSONField("stats", &stats)
		sum.Cpu += stats.Cpu
		sum.Mem += stats.Mem
		sum.MemUsed += stats.MemUsed
		sum.MemPct += stats.MemPct
		sum.MemBuffCache += stats.MemBuffCache
		sum.Swap += stats.Swap
		sum.SwapUsed += stats.SwapUsed
		sum.DiskTotal += stats.DiskTotal
		sum.DiskUsed += stats.DiskUsed
		sum.DiskPct += stats.DiskPct
		sum.DiskReadPs += stats.DiskReadPs
		sum.DiskWritePs += stats.DiskWritePs
		sum.NetworkSent += stats.NetworkSent
		sum.NetworkRecv += stats.NetworkRecv
		// add temps to sum
		if stats.Temperatures != nil {
			tempCount++
			for key, value := range stats.Temperatures {
				if _, ok := sum.Temperatures[key]; !ok {
					sum.Temperatures[key] = 0
				}
				sum.Temperatures[key] += value
			}
		}
		// add extra fs to sum
		if stats.ExtraFs != nil {
			for key, value := range stats.ExtraFs {
				if _, ok := sum.ExtraFs[key]; !ok {
					sum.ExtraFs[key] = &system.FsStats{}
				}
				sum.ExtraFs[key].DiskTotal += value.DiskTotal
				sum.ExtraFs[key].DiskUsed += value.DiskUsed
				sum.ExtraFs[key].DiskWritePs += value.DiskWritePs
				sum.ExtraFs[key].DiskReadPs += value.DiskReadPs
			}
		}
	}

	stats = system.Stats{
		Cpu:          twoDecimals(sum.Cpu / count),
		Mem:          twoDecimals(sum.Mem / count),
		MemUsed:      twoDecimals(sum.MemUsed / count),
		MemPct:       twoDecimals(sum.MemPct / count),
		MemBuffCache: twoDecimals(sum.MemBuffCache / count),
		Swap:         twoDecimals(sum.Swap / count),
		SwapUsed:     twoDecimals(sum.SwapUsed / count),
		DiskTotal:    twoDecimals(sum.DiskTotal / count),
		DiskUsed:     twoDecimals(sum.DiskUsed / count),
		DiskPct:      twoDecimals(sum.DiskPct / count),
		DiskReadPs:   twoDecimals(sum.DiskReadPs / count),
		DiskWritePs:  twoDecimals(sum.DiskWritePs / count),
		NetworkSent:  twoDecimals(sum.NetworkSent / count),
		NetworkRecv:  twoDecimals(sum.NetworkRecv / count),
	}

	if len(sum.Temperatures) != 0 {
		stats.Temperatures = make(map[string]float64)
		for key, value := range sum.Temperatures {
			stats.Temperatures[key] = twoDecimals(value / tempCount)
		}
	}

	if len(sum.ExtraFs) != 0 {
		stats.ExtraFs = make(map[string]*system.FsStats)
		for key, value := range sum.ExtraFs {
			stats.ExtraFs[key] = &system.FsStats{
				DiskTotal:   twoDecimals(value.DiskTotal / count),
				DiskUsed:    twoDecimals(value.DiskUsed / count),
				DiskWritePs: twoDecimals(value.DiskWritePs / count),
				DiskReadPs:  twoDecimals(value.DiskReadPs / count),
			}
		}
	}

	return stats
}

// Calculate the average stats of a list of container_stats records
func (rm *RecordManager) AverageContainerStats(records []*models.Record) (stats []container.Stats) {
	sums := make(map[string]*container.Stats)
	count := float64(len(records))

	var containerStats []container.Stats
	for _, record := range records {
		record.UnmarshalJSONField("stats", &containerStats)
		for _, stat := range containerStats {
			if _, ok := sums[stat.Name]; !ok {
				sums[stat.Name] = &container.Stats{Name: stat.Name, Cpu: 0, Mem: 0}
			}
			sums[stat.Name].Cpu += stat.Cpu
			sums[stat.Name].Mem += stat.Mem
			sums[stat.Name].NetworkSent += stat.NetworkSent
			sums[stat.Name].NetworkRecv += stat.NetworkRecv
		}
	}

	for _, value := range sums {
		stats = append(stats, container.Stats{
			Name:        value.Name,
			Cpu:         twoDecimals(value.Cpu / count),
			Mem:         twoDecimals(value.Mem / count),
			NetworkSent: twoDecimals(value.NetworkSent / count),
			NetworkRecv: twoDecimals(value.NetworkRecv / count),
		})
	}
	return stats
}

func (rm *RecordManager) DeleteOldRecords() {
	// start := time.Now()
	collections := []string{"system_stats", "container_stats"}
	recordData := []RecordDeletionData{
		{
			recordType: "1m",
			retention:  time.Hour,
		},
		{
			recordType: "10m",
			retention:  12 * time.Hour,
		},
		{
			recordType: "20m",
			retention:  24 * time.Hour,
		},
		{
			recordType: "120m",
			retention:  7 * 24 * time.Hour,
		},
		{
			recordType: "480m",
			retention:  30 * 24 * time.Hour,
		},
	}
	rm.app.Dao().RunInTransaction(func(txDao *daos.Dao) error {
		for _, recordData := range recordData {
			exp := dbx.NewExp(
				"type = {:type} AND created < {:created}",
				dbx.Params{"type": recordData.recordType, "created": time.Now().UTC().Add(-recordData.retention)},
			)
			for _, collectionSlug := range collections {
				collectionRecords, err := txDao.FindRecordsByExpr(collectionSlug, exp)
				if err != nil {
					return err
				}
				for _, record := range collectionRecords {
					err := txDao.DeleteRecord(record)
					if err != nil {
						rm.app.Logger().Error("Failed to delete records", "err", err.Error())
						return err
					}
				}
			}
		}
		return nil
	})
	// log.Println("finished deleting old records", "time (ms)", time.Since(start).Milliseconds())
}

/* Round float to two decimals */
func twoDecimals(value float64) float64 {
	return math.Round(value*100) / 100
}
