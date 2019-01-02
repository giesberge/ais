// Package ais provides types and methods for conducting data science
// on signals generated by maritime entities radiating from an Automated
// Identification System (AIS) transponder as mandated by the International
// Maritime Organization (IMO) for all vessels over 300 gross tons and all
// passenger vessels.
package ais

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"hash"
	"hash/fnv"
	"io"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/FATHOM5/haversine"
	"github.com/mmcloughlin/geohash"
)

// TimeLayout is the timestamp format for the MarineCadastre.gov
// AIS data available from the U.S. Government.  An example
// timestamp from the data set is `2017-12-05T00:01:14`.  This layout is
// designed to be passed to the time.Parse function as the layout
// string.
const TimeLayout = `2006-01-02T15:04:05`

// Unexported flushThreshhold is the number of records that a csv.Writer
// will write to memory before being flushed.
const flushThreshold = 250000

// ErrEmptySet is the error returned by Subset variants when there are no records
// in the returned *RecordSet because nothing matched the selection criteria.
// Functions should only return ErrEmptySet when all processing occurred successfully,
// but the subset criteria provided no matches to return.
var ErrEmptySet = errors.New("ErrEmptySet")

// Matching provides an interface to pass into the Subset and LimitSubset functions
// of a RecordSet.
type Matching interface {
	Match(*Record) (bool, error)
}

// Match is the function signature for the argument to ais.Matching
// used to match Records.  The variadic argument indices indicate the
// index numbers in the record for the fields that will be compared.
// type Match func(rec *Record) bool

// Vessel is a struct for the identifying information about a specific
// ship in an AIS dataset.  NOTE: REFINEMENT OF PACKAGE AIS WILL
// INCORPORATE MORE OF THE SHIP IDENTIFYING DATA COMMENTED OUT IN THIS
// MINIMALLY VIABLE IMPLEMENTATION.
type Vessel struct {
	MMSI       string
	VesselName string
	// IMO        string
	// Callsign   string
	// VesselType string
	// Length     string
	// Width      string
	// Draft      string
}

// VesselSet is a set of unique vessels usually obtained by the return
// value of RecordSet.UniqueVessels().  For each Record of a Vessel in the
// set the int value of the VesselSet is incremented
type VesselSet map[Vessel]int

// Field is an abstraction for string values that are read from and
// written to AIS Records.
type Field string

// Generator is the interface that is implemented to create a new Field
// from the index values of existing Fields in a Record.  The receiver for
// Generator should be a pointer in order to avoid creating a copy of the
// Record when Generate is called millions of times iterating over a large
// RecordSet.  Concrete implementation of the Generator interface are
// required arguments to RecordSet.AppendField(...).
type Generator interface {
	Generate(rec Record, index ...int) (Field, error)
}

// Geohasher is the base type for implementing the Generator interface to
// append a github.com/mccloughlin/geohash to each Record in the RecordSet.
// Pass NewGeohasher(rs *Recordset) as the gen argument of RecordSet.AppendField to
// add a geohash to a RecordSet.
type Geohasher RecordSet

// NewGeohasher returns a pointer to a new Geohasher.
func NewGeohasher(rs *RecordSet) *Geohasher {
	g := Geohasher(*rs)
	return &g
}

// Generate imlements the Generator interface to create a geohash Field.  The
// returned geohash is accurate to 22 bits of precision which corresponds to
// about .1 degree differences in lattitude and longitude.  The index values for
// the variadic function on a *Geohasher must be the index of "LAT" and "LON"
// in the rec.  Field will come back nil for any non-nil error returned.
func (g *Geohasher) Generate(rec Record, index ...int) (Field, error) {
	if len(index) != 2 {
		return "", fmt.Errorf("geohash: generate: len(index) must equal" +
			" 2 where the first int is the index of `LAT` and the second int is the index of `LON`")
	}
	indexLat, indexLon := index[0], index[1]

	// From these values create a geohash and return it
	lat, err := rec.ParseFloat(indexLat)
	if err != nil {
		return "", fmt.Errorf("geohash: unable to parse lat")
	}
	lon, err := rec.ParseFloat(indexLon)
	if err != nil {
		return "", fmt.Errorf("geohash: unable to parse lon")
	}
	hash := geohash.EncodeIntWithPrecision(lat, lon, uint(22))
	return Field(fmt.Sprintf("%#x", hash)), nil
}

// RecordSet is an the high level interface to deal with comma
// separated value files of AIS records. A RecordSet is not usually
// constructed from the struct.  Use NewRecordSet() to create an
// empty set, or OpenRecordSet(filename) to read a file on disk.
type RecordSet struct {
	r     *csv.Reader   // internally held csv pointer
	w     *csv.Writer   // internally held csv pointer
	h     Headers       // Headers used to parse each Record
	data  io.ReadWriter // client provided io interface
	first *Record       // accessible only by package functions
	stash *Record       // stashed Record from a client Read() but not yet used
}

// NewRecordSet returns a *Recordset that has an in-memory data buffer for
// the underlying Records that may be written to it.  Additionally, the new
// *Recordset is configured so that the encoding/csv objects it uses internally
// has LazyQuotes = true and and Comment = '#'.
func NewRecordSet() *RecordSet {
	rs := new(RecordSet)

	buf := bytes.Buffer{}
	rs.data = &buf
	rs.r = csv.NewReader(&buf)
	rs.w = csv.NewWriter(&buf)

	rs.r.LazyQuotes = true
	rs.r.Comment = '#'

	return rs
}

// OpenRecordSet takes the filename of an ais data file as its input.
// It returns a pointer to the RecordSet and a nil error upon successfully
// validating that the file can be read by an encoding/csv Reader. It returns
// a nil Recordset on any non-nil error.
func OpenRecordSet(filename string) (*RecordSet, error) {
	rs := NewRecordSet()

	f, err := os.OpenFile(filename, os.O_RDWR, 0666) // 0666 - Read Write
	if err != nil {
		return nil, fmt.Errorf("open recordset: %v", err)
	}
	rs.data = f
	rs.r = csv.NewReader(f)
	rs.r.LazyQuotes = true
	rs.r.Comment = '#'

	rs.w = csv.NewWriter(f)

	// The first non-comment line of a valid ais datafile should contain the headers.
	// The following Read() command also advances the file pointer so that
	// it now points at the first data line.
	var h Headers
	h.Fields, err = rs.r.Read()
	if err != nil {
		return nil, fmt.Errorf("open recordset: %v", err)
	}
	rs.h = h

	return rs, nil
}

// SetHeaders provides the expected interface to a RecordSet
func (rs *RecordSet) SetHeaders(h Headers) {
	rs.h = h
}

// Read calls Read() on the csv.Reader held by the RecordSet and returns a
// Record.  The idiomatic way to iterate over a recordset comes from the
// same idiom to read a file using encoding/csv.
func (rs *RecordSet) Read() (*Record, error) {
	// When Read is called by clients they want the first Record. If that
	// Record has already been read by internal packages return the one that
	// was already read internally.
	if rec := rs.first; rec != nil {
		rs.first = nil
		return rec, nil
	}

	// Clients may Read() a Record but not use it and want to get that same
	// Record back on the next call to Read().  Stash allows this functionality
	// to work.
	if rec := rs.stash; rec != nil {
		rs.stash = nil
		return rec, nil
	}

	r, err := rs.r.Read()
	if err == io.EOF {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("recordset read: %v", err)
	}
	rec := Record(r)
	return &rec, nil
}

// ReadFirst is an unexported method used by various internal packages
// to get the first line of the RecordSet.
func (rs *RecordSet) readFirst() (*Record, error) {
	if rs.first != nil {
		return rs.first, nil
	}
	r, err := rs.r.Read()
	if err != nil {
		return nil, err
	}
	rec := Record(r)
	rs.first = &rec
	return rs.first, nil
}

// Stash allows a client to take Record that has been previously retrieved
// through Read() and ensure the next call to Read() returns this same
// Record.
func (rs *RecordSet) Stash(rec *Record) {
	rs.stash = rec
}

// Write calls Write() on the csv.Writer held by the RecordSet and returns an
// error.  The error is nil on a successful write.  Flush() should be called at
// the end of necessary Write() calls to ensure the IO buffer flushed.
func (rs *RecordSet) Write(rec Record) error {
	err := rs.w.Write(rec)
	return err
}

// Flush empties the buffer in the underlying csv.Writer held by the RecordSet
// and returns any error that has occurred in a previous write or flush.
func (rs *RecordSet) Flush() error {
	rs.w.Flush()
	err := rs.w.Error()
	return err
}

// AppendField calls the Generator on each Record in the RecordSet and adds
// the resulting Field to each record under the newField provided as the
// argument.  The requiredHeaders argument is a []string of the required Headers
// that must be present in the RecordSet in order for Generator to be successful.
// If no errors are encournterd it returns a pointer to a new *RecordSet and a
// nil value for error.  If there is an error it will return a nil value for
// the *RecordSet and an error.
func (rs *RecordSet) AppendField(newField string, requiredHeaders []string, gen Generator) (*RecordSet, error) {
	rs2 := NewRecordSet()

	h := rs.Headers()
	h.Fields = append(h.Fields, newField)
	rs2.SetHeaders(h)

	// Find the index values for the Generator
	var indices []int
	for _, target := range requiredHeaders {
		index, ok := rs.Headers().Contains(target)
		if !ok {
			return nil, fmt.Errorf("append: headers does not contain %s", target)
		}
		indices = append(indices, index)
	}

	// Iterate over the records
	written := 0
	for {
		var rec Record
		rec, err := rs.r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("append: read error on csv file: %v", err)
		}

		field, err := gen.Generate(rec, indices...)
		if err != nil {
			return nil, fmt.Errorf("appendfield: generate: %v", err)
		}
		rec = append(rec, string(field))
		err = rs2.Write(rec)
		if err != nil {
			return nil, fmt.Errorf("appendfield: csv write error: %v", err)
		}
		written++
		if written%flushThreshold == 0 {
			err := rs2.Flush()
			if err != nil {
				return nil, fmt.Errorf("appendfield: csv flush error: %v", err)
			}
		}

	}
	err := rs2.Flush()
	if err != nil {
		return nil, fmt.Errorf("appendfield: csv flush error: %v", err)
	}
	return rs2, nil
}

// Close calls close on the unexported RecordSet data handle.
// It is the responsibility of the RecordSet user to
// call close.  This is usually accomplished by a call to
//      defer rs.Close()
// immediately after creating a NewRecordSet.
func (rs *RecordSet) Close() error {
	if rs.data == nil {
		return nil
	}
	// Use reflection to determine if the underlying io.ReadWriter
	// can call Close()
	closerType := reflect.TypeOf((*io.Closer)(nil)).Elem()

	if reflect.TypeOf(rs.data).Implements(closerType) {
		v := reflect.ValueOf(rs.data).MethodByName("Close").Call(nil)
		if v[0].Interface() == (error)(nil) {
			return nil
		}
		err := v[0].Interface().(error)
		return fmt.Errorf("recordset close: %v", err)
	}

	// no-op for types that do not implement close
	return nil
}

// Headers returns the encapsulated headers data of the Recordset
func (rs *RecordSet) Headers() Headers { return rs.h }

// Save writes the RecordSet to disk in the filename provided
func (rs *RecordSet) Save(name string) error {
	var err error
	rs.data, err = os.Create(name)
	if err != nil {
		return fmt.Errorf("recordset save: %v", err)
	}
	rs.w = csv.NewWriter(rs.data) // FYI - csv uses bufio.NewWriter internally
	rs.Write(rs.h.Fields)

	for {
		rec, err := rs.r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("recordset save: read error on csv file: %v", err)
		}
		rs.Write(rec)
	}
	err = rs.Flush()
	if err != nil {
		return fmt.Errorf("recordset save: flush error: %v", err)
	}

	return nil
}

// SubsetLimit returns a pointer to a new RecordSet with the first n records that
// return true from calls to Match(*Record) (bool, error) on the provided argument m
// that implements the Matching interface.
// Returns nil for the *RecordSet when error is non-nil.
// For n values less than zero, SubsetLimit will return all matches in the set.
//
// SubsetLimit also implement a bool argument, multipass, that will reset the read
// pointer in the RecordSet to the beginning of the data when set to true.  This has two
// important impacts.  First, it allows the same rs receiver to be used multiple times
// in a row because the read pointer is reset each time after hitting EOF.  Second, it
// has a significant performance penalty when dealing with a RecordSet of about one
// million or more records.  When performance impacts from setting multipass to true
// outweigh the convenience of additional boilerplate code it is quite helpful.  In
// situations where it is causing an issue use rs.Close() and then OpenRecordSet(filename)
// to get a fresh copy of the data.
func (rs *RecordSet) SubsetLimit(m Matching, n int, multipass bool) (*RecordSet, error) {
	rs2 := NewRecordSet()
	rs2.SetHeaders(rs.Headers())

	// In order to reset the read pointer of rs to the same data it was pointing at
	// when entering the function we create a new buffer and write all Reads to it.
	// Then point rs.r to this buffer at the end of the function. A more (MUCH MORE)
	// efficient solution would probably be controlling the Seek() value of the underlying
	// decriptor, but csv.Reader does not expose this pointer.
	copyBuf := &bytes.Buffer{}
	copyWriter := bufio.NewWriter(copyBuf)

	recordsLeftToWrite := n
	for recordsLeftToWrite != 0 {
		var rec *Record
		rec, err := rs.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("subset: read error on csv file: %v", err)
		}

		// This step is a SIGNFICANT performance penalty, but helpful in scenarios when
		// the underlying file cannot be reopened.
		if multipass {
			copyWriter.Write(rec.Data())
		}

		match, err := m.Match(rec)
		if err != nil {
			return nil, err
		}

		if match {
			err := rs2.Write(*rec)
			if err != nil {
				return nil, fmt.Errorf("subset: csv write error: %v", err)
			}
			recordsLeftToWrite--
			if recordsLeftToWrite%flushThreshold == 0 {
				err := rs2.Flush()
				if err != nil {
					return nil, fmt.Errorf("subset: csv flush error: %v", err)
				}
			}
		}
	}
	err := rs2.Flush()
	if err != nil {
		return nil, fmt.Errorf("subset: csv flush error: %v", err)
	}

	if recordsLeftToWrite == n { // no change, therefore no records written
		return rs2, ErrEmptySet
	}
	if multipass {
		copyWriter.Flush()
		rs.r = csv.NewReader(copyBuf)
	}
	return rs2, nil
}

// Subset returns a pointer to a new *RecordSet that contains all of the records that
// return true from calls to Match(*Record) (bool, error) on the provided argument m
// that implements the Matching interface.
// Returns nil for the *RecordSet when error is non-nil.
func (rs *RecordSet) Subset(m Matching) (*RecordSet, error) {
	return rs.SubsetLimit(m, -1, false)
}

// UniqueVessels returns a VesselMap, map[Vessel]int, that includes a unique key for
// each Vessel in the RecordSet.  The value of each key is the number of Records for
// that Vessel in the data.
func (rs *RecordSet) UniqueVessels() (VesselSet, error) {
	return rs.UniqueVesselsMulti(false)
}

// UniqueVesselsMulti provides an option to control whether the RecordSet read pointer
// is returned to the top of the file.  Using this option has a significant performance
// cost and is not recommended for any RecordSet with more than one million records.
// However, setting this version to true is valuable when the returned VesselMap is going
// to be used for additional queries on the same receiver. For example, ranging over
// the returned VesselSet to create a Subset of data for each ship requires reusing the
// rs reciver in most cases.
func (rs *RecordSet) UniqueVesselsMulti(multipass bool) (VesselSet, error) {
	vs := make(VesselSet)
	var defaultVesselName = "no VesselName header"

	mmsiIndex, ok := rs.Headers().Contains("MMSI")
	if !ok {
		return nil, fmt.Errorf("unique vessels: recordset does not contain MMSI header")
	}
	vesselNameIndex, okVesselName := rs.Headers().Contains("VesselName")

	var rec *Record
	var err error

	// In order to reset the read pointer of rs to the same data it was pointing at
	// when entering the function we create a new buffer and write all Reads to it.
	// Then point rs.r to this buffer at the end of the function. A more (MUCH MORE)
	// efficient solution would probably be controlling the Seek() value of the underlying
	// decriptor, but csv.Reader does not expose this pointer.
	copyBuf := &bytes.Buffer{}
	copyWriter := bufio.NewWriter(copyBuf)

	for {
		rec, err = rs.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("unique vessel: read error on csv file: %v", err)
		}
		if multipass {
			copyWriter.Write(rec.Data())
		}

		if okVesselName {
			vs[Vessel{MMSI: (*rec)[mmsiIndex], VesselName: (*rec)[vesselNameIndex]}]++
		} else {
			vs[Vessel{MMSI: (*rec)[mmsiIndex], VesselName: defaultVesselName}]++
		}
	}
	if multipass {
		copyWriter.Flush()
		rs.r = csv.NewReader(copyBuf)
	}
	return vs, nil
}

// SortByTime returns a pointer to a new RecordSet sorted in ascending order
// by BaseDateTime.
func (rs *RecordSet) SortByTime() (*RecordSet, error) {
	rs2 := NewRecordSet()
	rs2.SetHeaders(rs.Headers())

	bt, err := NewByTimestamp(rs)
	if err != nil {
		return nil, fmt.Errorf("sortbytime: %v", err)
	}

	sort.Sort(bt)

	// Write the reports to the new RecordSet
	// NOTE: Headers are written only when the RecordSet is saved to disk
	written := 0
	for _, rec := range *bt.data {
		rs2.Write(rec)
		written++
		if written%flushThreshold == 0 {
			err := rs2.Flush()
			if err != nil {
				return nil, fmt.Errorf("sortbytime: flush error writing to new recordset: %v", err)
			}
		}
	}
	err = rs2.Flush()
	if err != nil {
		return nil, fmt.Errorf("sortbytime: flush error writing to new recordset: %v", err)
	}

	return rs2, nil
}

// Box provides a type with min and max values for latitude and longitude, and Box
// implements the Matching interface.  This provides a convenient way to create a
// Box and pass the new object to Subset in order to get a *RecordSet defined
// with a geographic boundary.  Box includes records that are on the border
// and at the vertices of the geographic boundary. Constructing a box also requires
// the index value for lattitude and longitude in a *Record.  These index values will be
// called in *Record.ParseFloat(index) from the Match method of a Box in order to
// see if the Record is in the Box.
type Box struct {
	MinLat, MaxLat, MinLon, MaxLon float64
	LatIndex, LonIndex             int
}

// Match implements the Matching interface for a Box.  Errors in the Match function
// can be caused by parse errors when converting string Record values into their
// typed values. When Match returns a non-nil error the bool value will be false.
func (b *Box) Match(rec *Record) (bool, error) {
	lat, err := rec.ParseFloat(b.LatIndex)
	if err != nil {
		return false, fmt.Errorf("unable to parse %v", (*rec)[b.LatIndex])
	}
	lon, err := rec.ParseFloat(b.LonIndex)
	if err != nil {
		return false, fmt.Errorf("unable to parse %v", (*rec)[b.LonIndex])
	}

	return lat >= b.MinLat && lat <= b.MaxLat && lon >= b.MinLon && lon <= b.MaxLon, nil
}

// ByTimestamp implements the sort.Interface for creating a RecordSet
// sorted by BaseDateTime. The ByTimestamp struct and its Len, Swap, and Less
// methods are exported in order to serve as examples for how to implement the
// sort.Interface for a RecordSet.  If you want to sort a RecordSet by time you
// do not need to call these methods.  Just call RecordSet.SortByTime() directly
// to take advantage of the implementation provided in the package.
type ByTimestamp struct {
	h    Headers
	data *[]Record
}

// NewByTimestamp returns a data structure suitable for sorting using
// the sort.Interface tools.
func NewByTimestamp(rs *RecordSet) (*ByTimestamp, error) {
	bt := new(ByTimestamp)
	bt.h = rs.Headers()

	// Read the data from the underlying Recordset into a slice
	var err error
	bt.data, err = rs.loadRecords()
	if err != nil {
		return nil, fmt.Errorf("new bytimestamp: unable to load data: %v", err)
	}

	return bt, nil
}

// Len function to implement the sort.Interface.
func (bt ByTimestamp) Len() int { return len(*bt.data) }

// Swap function to implement the sort.Interface.
func (bt ByTimestamp) Swap(i, j int) {
	(*bt.data)[i], (*bt.data)[j] = (*bt.data)[j], (*bt.data)[i]
}

//Less function to implement the sort.Interface.
func (bt ByTimestamp) Less(i, j int) bool {
	timeIndex, ok := bt.h.Contains("BaseDateTime")
	if !ok {
		panic("bytimestamp: less: headers does not contain BaseDateTime")
	}
	t1, err := time.Parse(TimeLayout, (*bt.data)[i][timeIndex])
	if err != nil {
		panic(err)
	}
	t2, err := time.Parse(TimeLayout, (*bt.data)[j][timeIndex])
	if err != nil {
		panic(err)
	}
	return t1.Before(t2)
}

// Unexported loadRecords reads the RecordSet into memory and returns a
// *[]Record and any error that occurred.  If err is non-nil then loadRecords
// returns nil for the *[]Record
func (rs *RecordSet) loadRecords() (*[]Record, error) {
	recs := new([]Record)

	record := new(Record)
	for {
		var err error
		record, err = rs.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}

		*recs = append(*recs, *record)
	}
	return recs, nil
}

// Headers are the field names for AIS data elements in a Record.
type Headers struct {
	// Fields is an encapsulated []string . It is initialized from the first
	// non-comment line of an AIS .csv file when ais.OpenRecordSet(filename string)
	// is called.
	Fields []string

	// DEPRECATED
	// dictionary is a map[fieldname]description composed of string values
	// usually created from a JSON file that contains
	// Definition structs for each of the fields in the set of ais.Headers.
	// dictionary map[string]string
}

// Contains returns the index of a specific header.  This provides
// a nice syntax ais.Headers().Contains("LAT") to ensure
// an ais.Record contains a specific field.  If the Headers do not
// contain the requested field ok is false.
func (h Headers) Contains(field string) (i int, ok bool) {
	for i, s := range h.Fields {
		if s == field {
			return i, true
		}
	}
	return 0, false
}

// HeaderMap is the returned map value for ContainsMulti. See the distance example
// for using the HeaderMap.
type HeaderMap struct {
	Present bool
	Idx     int
}

// ContainsMulti returns a map[string]int where the map keys are the
// field names and the int values are the index positions of the various
// fields in the Headers set. If there is an error determining an index
// position for any field then idxMap returns nil and ok is false.  Users
// should always check for !ok and handle accordingly.
func (h Headers) ContainsMulti(fields ...string) (idxMap map[string]HeaderMap, ok bool) {
	idxMap = make(map[string]HeaderMap)
	for _, f := range fields { // note range over argument not receiver
		idx, ok := h.Contains(f)
		if ok {
			idxMap[f] = HeaderMap{Present: true, Idx: idx}
		} else {
			return nil, false
		}
	}
	return idxMap, true
}

// String satisfies the fmt.Stringer interface for Headers.  It pretty prints
// each index value and header, one line per header.
func (h Headers) String() string {
	const pad = ' ' //padding character for prety print

	b := new(bytes.Buffer)
	w := tabwriter.NewWriter(b, 0, 0, 2, pad, 0)

	fmt.Fprintf(w, "Index\tHeader\n")

	// For each header pretty print its name and index
	for i, header := range h.Fields {
		header = strings.TrimSpace(header)
		fmt.Fprintf(w, "%d\t%s\n", i, header)
	}
	w.Flush()

	return b.String()
}

// Equals supports comparison testing of two Headers sets.
func (h Headers) Equals(h2 Headers) bool {
	if (h.Fields == nil) != (h2.Fields == nil) {
		return false
	}

	if len(h.Fields) != len(h2.Fields) {
		return false
	}

	for i, f := range h.Fields {
		if f != h2.Fields[i] {
			return false
		}
	}

	return true
}

// Record wraps the return value from a csv.Reader because many publicly
// available data sources provide AIS records in large csv files. The Record
// type and its associate methods allow clients of the package to deal
// directly with the abtraction of individual AIS records and handle the
// csv file read/write operations internally.
type Record []string

// Hash returns a 64 bit hash/fnv of the Record
func (r Record) Hash() uint64 {
	var h64 hash.Hash64
	h64 = fnv.New64a()
	h64.Write(r.Data())
	return h64.Sum64()
}

// Data returns the underlying []string in a Record as a []byte
func (r Record) Data() []byte {
	var b bytes.Buffer
	b.WriteString(strings.Join([]string(r), ","))
	b.WriteString("\n")
	return b.Bytes()
}

// Distance calculates the haversine distance between two AIS records that
// contain a latitude and longitude measurement identified by their index
// number in the Record slice.
func (r Record) Distance(r2 Record, latIndex, lonIndex int) (nm float64, err error) {
	latP, _ := r.ParseFloat(latIndex)
	lonP, _ := r.ParseFloat(lonIndex)
	latQ, _ := r2.ParseFloat(latIndex)
	lonQ, _ := r2.ParseFloat(lonIndex)
	p := haversine.Coord{Lat: latP, Lon: lonP}
	q := haversine.Coord{Lat: latQ, Lon: lonQ}
	nm = haversine.Distance(p, q)
	return nm, nil
}

// ParseFloat wraps strconv.ParseFloat with a method to return a
// float64 from the index value of a field in the AIS Record.
// Useful for getting a LAT, LON, SOG or other numeric value
// from an ais.Record.
func (r Record) ParseFloat(index int) (float64, error) {
	f, err := strconv.ParseFloat(r[index], 64)
	if err != nil {
		return 0, err
	}
	return f, nil
}

// ParseInt wraps strconv.ParseInt with a method to return an
// Int64 from the index value of a field in the AIS Record.
// Useful for getting int values from the Records such as MMSI
// and IMO number.
func (r Record) ParseInt(index int) (int64, error) {
	i, err := strconv.ParseInt(r[index], 10, 64)
	if err != nil {
		return 0, err
	}
	return i, nil
}

// ParseTime wraps time.Parse with a method to return a time.Time
// from the index value of a field in the AIS Record.
// Useful for converting the BaseDateTime from the Record.
// NOTE: FUTURE VERSIONS OF THIS METHOD SHOULD NOT RELY ON A PACKAGE
// CONSTANT FOR THE LAYOUT FIELD. THIS FIELD SHOULD BE INFERRED FROM
// A LIST OF FORMATS SEEN IN COMMON DATASOURCES.
func (r Record) ParseTime(index int) (time.Time, error) {
	t, err := time.Parse(TimeLayout, r[index])
	if err != nil {
		return time.Time{}, err
	}
	return t, nil
}

// Value returns the record value for the []string index. For out out bounds idx
// arguments or other errors Value returns an empty string for val and false for ok.
func (r *Record) Value(idx int) (val string, ok bool) {
	for idx < 0 {
		return "", false
	}
	if idx > len(*r)-1 {
		return "", false
	}
	return (*r)[idx], true
}

// ValueFrom HeaderMap returns the record value decribed in the HeaderMap. The argument
// is a HeaderMap and normal usage has the nice syntax rec.ValueFrom(idxMap["LAT"]),
// where idxMap is the returned value from ContainsMulti(...). Returns an empty
// string and false when hm.Present == false.
func (r *Record) ValueFrom(hm HeaderMap) (val string, ok bool) {
	if !hm.Present {
		return "", false
	}
	if hm.Idx < 0 || hm.Idx > len(*r)-1 {
		return "", false
	}
	return (*r)[hm.Idx], true
}

// Parse converts the string record values into an ais.Report.  It
// takes a set of headers as arguments to identify the fields in
// the Record.
// NOTE 1: FUTURE VERSIONS MAY ALSO RETURN A CORRELATION STRUCT SO
// USERS CAN SEE THE FIELD NAMES THAT WERE USED TO MAKE ASSIGNMENTS
// TO THE REPORT VALUES.  THIS WOULD BE HELPFUL WHEN THERE ARE MULTIPLE
// STRING NAMES TO REPRESENT THE SAME RECORD FIELD.  FOR EXAMPLE, SOME
// DATASETS USE "TIME" INSTEAD OF THE MARINECADASTRE USE OF THE
// FIELD NAME "BASEDATETIME" BUT BOTH SHOULD MAP TO THE "TIMESTAMP" FIELD
// OF REPORT.
// NOTE 2: FUTURE VERSION OF THIS METHOD SHOULD ITERATE OVER THE REPORT
// STRUCT AND FIND THE REQUIRED FIELDS, NOT RELY ON THE HARDCODED VERSION
// PRESENTED IN THE FIRST FEW LINES OF THIS FUNCTION WHERE I HAVE A
// MINIMALLY VIABLE IMPLEMENTATION.
// func (r Record) Parse(h Headers) (Report, error) {
// 	requiredFields := []string{"MMSI", "BaseDateTime", "LAT", "LON"}
// 	fields := make(map[string]int)

// 	for _, field := range requiredFields {
// 		j, ok := h.Contains(field)
// 		if !ok {
// 			return Report{}, fmt.Errorf("record parse: passed headers does not contain required field %s", field)
// 		}
// 		fields[field] = j
// 	}
// 	mmsi, err := r.ParseInt(fields["MMSI"])
// 	if err != nil {
// 		return Report{}, fmt.Errorf("record parse: unable to parse MMSI: %s", err)
// 	}
// 	t, err := r.ParseTime(fields["BaseDateTime"])
// 	if err != nil {
// 		return Report{}, fmt.Errorf("record parse: unable to parse BaseDateTime: %s", err)
// 	}
// 	lat, err := r.ParseFloat(fields["LAT"])
// 	if err != nil {
// 		return Report{}, fmt.Errorf("record parse: unable to parse LAT: %s", err)
// 	}
// 	lon, err := r.ParseFloat(fields["LON"])
// 	if err != nil {
// 		return Report{}, fmt.Errorf("record parse: unable to parse LON: %s", err)
// 	}

// 	return Report{
// 		MMSI:      mmsi,
// 		Lat:       lat,
// 		Lon:       lon,
// 		Timestamp: t,
// 	}, nil

// }

// Report is the converted string data from an ais.Record into a series
// of typed values suitable for data analytics.
// NOTE: THIS SET OF FIELDS WILL EVOLVE OVER TIME TO SUPPORT A LARGER
// SET OF USE CASES AND ANALYTICS.  DO NOT RELY ON THE ORDER OF THE
// FIELDS IN THIS TYPE.
// type Report struct {
// 	MMSI      int64
// 	Lat       float64
// 	Lon       float64
// 	Timestamp time.Time
// 	data      []interface{}
// }

// Data returns the Report fields in a slice of interface values.
// func (rep Report) Data() []interface{} {
// 	rep.data = []interface{}{
// 		int64(rep.MMSI),
// 		time.Time(rep.Timestamp),
// 		float64(rep.Lat),
// 		float64(rep.Lon),
// 	}
// 	return rep.data
// }
