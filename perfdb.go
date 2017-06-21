package main

import (
	"bufio"
	"fmt"
	"io/ioutil"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/pmylund/go-cache"
)

const (
	dataFileExt = ".data"
)

type Sample struct {
	ts int64
	v  float64
}

type perfDB struct {
	baseDir string
	cache   cache.Cache
	mu      sync.Mutex
}

func newPerfDB(baseDir string) (*perfDB, error) {
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		logger.Critical("Failed to initialize datastore: %s", err)
		return nil, err
	}
	c := cache.New(cache.NoExpiration, cache.NoExpiration)
	return &perfDB{baseDir, *c, sync.Mutex{}}, nil
}

func (pdb *perfDB) listDatabases() ([]string, error) {
	files, err := ioutil.ReadDir(pdb.baseDir)
	if err != nil {
		return nil, err
	}
	databases := []string{}
	for _, f := range files {
		databases = append(databases, f.Name())
	}
	return databases, nil
}

func (pdb *perfDB) listSources(dbname string) ([]string, error) {
	dataDir := filepath.Join(pdb.baseDir, dbname)
	files, err := ioutil.ReadDir(dataDir)
	if err != nil {
		return nil, err
	}
	collections := []string{}
	for _, f := range files {
		collections = append(collections, f.Name())
	}
	return collections, nil
}

func (pdb *perfDB) listMetrics(dbname, collection string) ([]string, error) {
	dataDir := filepath.Join(pdb.baseDir, dbname, collection)
	files, err := ioutil.ReadDir(dataDir)
	if err != nil {
		return nil, err
	}
	metrics := []string{}
	for _, f := range files {
		fname := strings.Replace(f.Name(), dataFileExt, "", 1)
		metrics = append(metrics, fname)
	}
	return metrics, nil
}

const tsOffset = 22

func (pdb *perfDB) addSample(dbname, collection, metric string, sample Sample) error {
	dataDir := filepath.Join(pdb.baseDir, dbname, collection)
	if err := os.MkdirAll(dataDir, 0775); err != nil {
		return err
	}

	dataFile := filepath.Join(dataDir, metric+dataFileExt)

	file, err := os.OpenFile(dataFile, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		logger.Critical(err)
		return err
	}
	defer file.Close()

	pdb.mu.Lock()
	defer pdb.mu.Unlock()
	if _, err := fmt.Fprintf(file, "%22d %025.12f\n", sample.ts, sample.v); err != nil {
		logger.Critical(err)
		return err
	}

	return nil
}

const bufferSize = 1000

func fullScan(fileName string, done <-chan struct{}) (<-chan string, <-chan error) {
	samples := make(chan string, bufferSize)
	errc := make(chan error, 1)

	go func() {
		defer close(samples)
		defer close(errc)

		file, err := os.Open(fileName)
		if err != nil {
			errc <- err
			return
		}
		defer file.Close()

		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			select {
			case samples <- scanner.Text():
			case <-done:
				return
			}
		}

		if err := scanner.Err(); err != nil {
			errc <- err
		}
	}()
	return samples, errc
}

func parseSamples(records <-chan string) (<-chan Sample, <-chan error) {
	samples := make(chan Sample, bufferSize)
	errc := make(chan error, 1)

	go func() {
		defer close(samples)
		defer close(errc)

		var err error
		var value float64
		var ts int64
		for record := range records {
			if value, err = strconv.ParseFloat(record[tsOffset+1:], 64); err != nil {
				errc <- err
				break
			}
			if ts, err = strconv.ParseInt(strings.TrimSpace(record[:tsOffset]), 10, 64); err != nil {
				errc <- err
				break
			}
			samples <- Sample{ts, value}
		}
	}()
	return samples, errc
}

func mergeErrors(errcs ...<-chan error) error {
	for _, errc := range errcs {
		if err := <-errc; err != nil {
			return err
		}
	}
	return nil
}

func (pdb *perfDB) getRawValues(dbname, collection, metric string) ([][]interface{}, error) {
	dataDir := filepath.Join(pdb.baseDir, dbname, collection)
	dataFile := filepath.Join(dataDir, metric+dataFileExt)

	done := make(chan struct{}, 1)
	rawSamples, rawErrors := fullScan(dataFile, done)
	parsedSamples, parsedErrors := parseSamples(rawSamples)

	values := [][]interface{}{}
	for sample := range parsedSamples {
		values = append(values, []interface{}{sample.ts, sample.v})
	}

	done <- struct{}{}
	if err := mergeErrors(rawErrors, parsedErrors); err != nil {
		return nil, err
	}
	return values, nil
}

func metricHash(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("%s%d", filePath, info.Size()), nil
}

func (pdb *perfDB) getSummary(dbname, collection, metric string) (map[string]interface{}, error) {
	var summary map[string]interface{}

	dataDir := filepath.Join(pdb.baseDir, dbname, collection)
	dataFile := filepath.Join(dataDir, metric+dataFileExt)

	hash, err := metricHash(dataFile)
	if err != nil {
		return nil, err
	}

	if cachedData, found := pdb.cache.Get(string(hash)); found {
		return cachedData.(map[string]interface{}), nil
	}

	done := make(chan struct{}, 1)
	defer close(done)

	rawSamples, rawErrors := fullScan(dataFile, done)
	parsedSamples, parsedErrors := parseSamples(rawSamples)

	values := []float64{}
	sum := 0.0
	for sample := range parsedSamples {
		sum += sample.v
		values = append(values, sample.v)
	}

	done <- struct{}{}
	if err := mergeErrors(rawErrors, parsedErrors); err != nil {
		return nil, err
	}

	count := len(values)
	sort.Float64s(values)

	summary = map[string]interface{}{
		"max":   values[count-1],
		"min":   values[0],
		"count": count,
		"avg":   sum / float64(count),
	}

	for _, percentile := range []float64{0.5, 0.8, 0.9, 0.95, 0.99, 0.999} {
		var pIdx int
		if count > 1 {
			pIdx = int(float64(count)*percentile) - 1
		}
		p := fmt.Sprintf("p%v", percentile*100)
		summary[p] = values[pIdx]
	}

	pdb.cache.Set(hash, summary, cache.NoExpiration)
	return summary, nil
}

type parsedSample struct {
	ts int64
	v  float64
}

func parseSamplesWithTimestamp(records <-chan string) (<-chan parsedSample, <-chan error) {
	samples := make(chan parsedSample, bufferSize)
	errc := make(chan error, 1)

	go func() {
		defer close(samples)
		defer close(errc)

		var err error
		var ts int64
		var value float64
		for record := range records {
			if value, err = strconv.ParseFloat(record[tsOffset+1:], 64); err != nil {
				errc <- err
				break
			}
			if ts, err = strconv.ParseInt(strings.TrimSpace(record[:tsOffset]), 10, 64); err != nil {
				errc <- err
				break
			}
			samples <- parsedSample{ts, value}
		}
	}()
	return samples, errc
}

func (pdb *perfDB) getHeatMap(dbname, collection, metric string) (*heatMap, error) {
	dataDir := filepath.Join(pdb.baseDir, dbname, collection)
	dataFile := filepath.Join(dataDir, metric+dataFileExt)

	hm := newHeatMap()
	hm.MinTS = int64(^uint64(0) >> 1)

	done := make(chan struct{}, 1)
	defer close(done)

	rawSamples, rawErrors := fullScan(dataFile, done)
	parsedSamples, parsedErrors := parseSamplesWithTimestamp(rawSamples)

	samples := []parsedSample{}
	for sample := range parsedSamples {
		hm.MaxValue = math.Max(hm.MaxValue, sample.v)
		if sample.ts < hm.MinTS {
			hm.MinTS = sample.ts
		} else if sample.ts > hm.MaxTS {
			hm.MaxTS = sample.ts
		}
		samples = append(samples, sample)
	}

	done <- struct{}{}
	if err := mergeErrors(rawErrors, parsedErrors); err != nil {
		return nil, err
	}

	for _, sample := range samples {
		x := math.Floor(heatMapWidth * float64(sample.ts-hm.MinTS) / float64(hm.MaxTS-hm.MinTS))
		y := math.Floor(heatMapHeight * sample.v / hm.MaxValue)
		if x == heatMapWidth {
			x--
		}
		if y == heatMapHeight {
			y--
		}
		hm.Map[int(y)][int(x)]++
		if hm.Map[int(y)][int(x)] > hm.maxDensity {
			hm.maxDensity = hm.Map[int(y)][int(x)]
		}
	}
	return hm, nil
}

const numBins = 6

func (pdb *perfDB) getHistogram(dbname, collection, metric string) (map[string]float64, error) {
	dataDir := filepath.Join(pdb.baseDir, dbname, collection)
	dataFile := filepath.Join(dataDir, metric+dataFileExt)

	done := make(chan struct{}, 1)
	defer close(done)

	rawSamples, rawErrors := fullScan(dataFile, done)
	parsedSamples, parsedErrors := parseSamples(rawSamples)

	values := []float64{}
	for sample := range parsedSamples {
		values = append(values, sample.v)
	}

	done <- struct{}{}
	if err := mergeErrors(rawErrors, parsedErrors); err != nil {
		return nil, err
	}

	sort.Float64s(values)
	count := int(float64(len(values)) * 0.99) // cut values above 99th percentile
	minValue := values[0]
	maxValue := values[count]
	delta := (maxValue - minValue) / numBins

	histogram := map[string]float64{}
	ranges := map[int]string{}
	for i := 0; i < numBins; i++ {
		lr := minValue + float64(i)*delta
		rr := lr + delta
		rname := fmt.Sprintf("%f - %f", lr, rr)
		ranges[i] = rname
		histogram[rname] = 0
	}

	incr := 100 / float64(count)
	for _, value := range values[:count] {
		i := int((value - minValue) / delta)
		if i >= numBins {
			i = numBins - 1
		}
		histogram[ranges[i]] += incr
	}

	return histogram, nil
}
