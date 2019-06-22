package tcglog

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
)

type Spec uint

type Options struct {
	EnableGrub           bool
	EfiVariableBootQuirk bool
}

var knownAlgorithms = map[AlgorithmId]uint16{
	AlgorithmSha1:   20,
	AlgorithmSha256: 32,
	AlgorithmSha384: 48,
	AlgorithmSha512: 64,
}

func isKnownAlgorithm(alg AlgorithmId) (out bool) {
	_, out = knownAlgorithms[alg]
	return
}

var zeroDigests = map[AlgorithmId][]byte{
	AlgorithmSha1:   make([]byte, knownAlgorithms[AlgorithmSha1]),
	AlgorithmSha256: make([]byte, knownAlgorithms[AlgorithmSha256]),
	AlgorithmSha384: make([]byte, knownAlgorithms[AlgorithmSha384]),
	AlgorithmSha512: make([]byte, knownAlgorithms[AlgorithmSha512])}

type stream interface {
	readNextEvent() (*Event, error)
}

const maxPCRIndex PCRIndex = 31

func isPCRIndexInRange(index PCRIndex) bool {
	return index <= maxPCRIndex
}

func wrapLogReadError(origErr error, partial bool) error {
	if origErr == io.EOF {
		if !partial {
			return origErr
		}
		origErr = io.ErrUnexpectedEOF
	}

	return &LogReadError{origErr}
}

type stream_1_2 struct {
	r         io.ReadSeeker
	options   Options
	byteOrder binary.ByteOrder
}

// https://trustedcomputinggroup.org/wp-content/uploads/TCG_PCClientImplementation_1-21_1_00.pdf
//  (section 11.1.1 "TCG_PCClientPCREventStruct Structure")
func (s *stream_1_2) readNextEvent() (*Event, error) {
	var pcrIndex PCRIndex
	if err := binary.Read(s.r, s.byteOrder, &pcrIndex); err != nil {
		return nil, wrapLogReadError(err, false)
	}

	if !isPCRIndexInRange(pcrIndex) {
		return nil, &PCRIndexOutOfRangeError{pcrIndex}
	}

	var eventType EventType
	if err := binary.Read(s.r, s.byteOrder, &eventType); err != nil {
		return nil, wrapLogReadError(err, true)
	}

	digest := make(Digest, knownAlgorithms[AlgorithmSha1])
	if _, err := s.r.Read(digest); err != nil {
		return nil, wrapLogReadError(err, true)
	}
	digests := make(DigestMap)
	digests[AlgorithmSha1] = digest

	var eventSize uint32
	if err := binary.Read(s.r, s.byteOrder, &eventSize); err != nil {
		return nil, wrapLogReadError(err, true)
	}

	event := make([]byte, eventSize)
	if _, err := io.ReadFull(s.r, event); err != nil {
		return nil, wrapLogReadError(err, true)
	}

	data, dataErr := makeEventData(pcrIndex, eventType, event, s.byteOrder, &s.options)

	return &Event{
		PCRIndex:  pcrIndex,
		EventType: eventType,
		Digests:   digests,
		Data:      data,
	}, dataErr
}

type stream_2 struct {
	r              io.ReadSeeker
	options        Options
	byteOrder      binary.ByteOrder
	algSizes       []EFISpecIdEventAlgorithmSize
	readFirstEvent bool
}

// https://trustedcomputinggroup.org/wp-content/uploads/TCG_PCClientSpecPlat_TPM_2p0_1p04_pub.pdf
//  (section 9.2.2 "TCG_PCR_EVENT2 Structure")
func (s *stream_2) readNextEvent() (*Event, error) {
	if !s.readFirstEvent {
		s.readFirstEvent = true
		stream := stream_1_2{r: s.r, byteOrder: s.byteOrder}
		return stream.readNextEvent()
	}

	var pcrIndex PCRIndex
	if err := binary.Read(s.r, s.byteOrder, &pcrIndex); err != nil {
		return nil, wrapLogReadError(err, false)
	}

	if !isPCRIndexInRange(pcrIndex) {
		return nil, &PCRIndexOutOfRangeError{pcrIndex}
	}

	var eventType EventType
	if err := binary.Read(s.r, s.byteOrder, &eventType); err != nil {
		return nil, wrapLogReadError(err, true)
	}

	var count uint32
	if err := binary.Read(s.r, s.byteOrder, &count); err != nil {
		return nil, wrapLogReadError(err, true)
	}

	digests := make(DigestMap)

	for i := uint32(0); i < count; i++ {
		var algorithmId AlgorithmId
		if err := binary.Read(s.r, s.byteOrder, &algorithmId); err != nil {
			return nil, wrapLogReadError(err, true)
		}

		var digestSize uint16
		var j int
		for j = 0; j < len(s.algSizes); j++ {
			if s.algSizes[j].AlgorithmId == algorithmId {
				digestSize = s.algSizes[j].DigestSize
				break
			}
		}

		if j == len(s.algSizes) {
			return nil, &UnrecognizedAlgorithmError{algorithmId}
		}

		digest := make(Digest, digestSize)
		if _, err := io.ReadFull(s.r, digest); err != nil {
			return nil, wrapLogReadError(err, true)
		}

		if _, exists := digests[algorithmId]; exists {
			return nil, &DuplicateDigestValueError{algorithmId}
		}
		digests[algorithmId] = digest
	}

	for _, algSize := range s.algSizes {
		if _, exists := digests[algSize.AlgorithmId]; !exists {
			return nil, &MissingDigestValueError{algSize.AlgorithmId}
		}
	}

	for alg, _ := range digests {
		if isKnownAlgorithm(alg) {
			continue
		}
		delete(digests, alg)
	}

	var eventSize uint32
	if err := binary.Read(s.r, s.byteOrder, &eventSize); err != nil {
		return nil, wrapLogReadError(err, true)
	}

	event := make([]byte, eventSize)
	if _, err := io.ReadFull(s.r, event); err != nil {
		return nil, wrapLogReadError(err, true)
	}

	data, dataErr := makeEventData(pcrIndex, eventType, event, s.byteOrder, &s.options)

	return &Event{
		PCRIndex:  pcrIndex,
		EventType: eventType,
		Digests:   digests,
		Data:      data,
	}, dataErr
}

func fixupSpecIdEvent(event *Event, algorithms []AlgorithmId) {
	if event.Data.(*SpecIdEventData).Spec != SpecEFI_2 {
		return
	}

	for _, alg := range algorithms {
		if alg == AlgorithmSha1 {
			continue
		}

		if _, ok := event.Digests[alg]; ok {
			continue
		}

		event.Digests[alg] = zeroDigests[alg]
	}
}

func isSpecIdEvent(event *Event) (out bool) {
	_, out = event.Data.(*SpecIdEventData)
	return
}

type Log struct {
	Spec         Spec
	Algorithms   []AlgorithmId
	byteOrder    binary.ByteOrder
	stream       stream
	failed       bool
	indexTracker map[PCRIndex]uint
}

func newLogFromReader(r io.ReadSeeker, options Options) (*Log, error) {
	start, err := r.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, err
	}

	var byteOrder binary.ByteOrder = binary.LittleEndian

	var stream stream = &stream_1_2{r: r, options: options, byteOrder: byteOrder}
	event, err := stream.readNextEvent()
	if event == nil {
		return nil, wrapLogReadError(err, true)
	}

	var spec Spec = SpecUnknown
	var algorithms []AlgorithmId
	specData, isSpecData := event.Data.(*SpecIdEventData)
	if isSpecData {
		spec = specData.Spec
	} else if _, isSpecErr := err.(*InvalidSpecIdEventError); isSpecErr {
		return nil, err
	}

	if spec == SpecEFI_2 {
		algorithms = make([]AlgorithmId, 0, len(specData.DigestSizes))
		for _, specAlgSize := range specData.DigestSizes {
			if isKnownAlgorithm(specAlgSize.AlgorithmId) {
				algorithms = append(algorithms, specAlgSize.AlgorithmId)
			}
		}
		stream = &stream_2{r: r,
			options:        options,
			byteOrder:      byteOrder,
			algSizes:       specData.DigestSizes,
			readFirstEvent: false}
	} else {
		algorithms = []AlgorithmId{AlgorithmSha1}
	}

	_, err = r.Seek(start, io.SeekStart)
	if err != nil {
		return nil, err
	}

	return &Log{Spec: spec,
		Algorithms:   algorithms,
		byteOrder:    byteOrder,
		stream:       stream,
		failed:       false,
		indexTracker: map[PCRIndex]uint{}}, nil
}

func (l *Log) nextEventInternal() (*Event, error) {
	if l.failed {
		return nil, &LogReadError{errors.New("log status inconsistent due to a previous error")}
	}

	event, err := l.stream.readNextEvent()
	if event == nil {
		if err != io.EOF {
			l.failed = true
		}
		return nil, err
	}

	if i, exists := l.indexTracker[event.PCRIndex]; exists {
		event.Index = i
		l.indexTracker[event.PCRIndex] = i + 1
	} else {
		event.Index = 0
		l.indexTracker[event.PCRIndex] = 1
	}

	if isSpecIdEvent(event) {
		fixupSpecIdEvent(event, l.Algorithms)
	}

	return event, err
}

func (l *Log) HasAlgorithm(alg AlgorithmId) bool {
	for _, a := range l.Algorithms {
		if a == alg {
			return true
		}
	}

	return false
}

func (l *Log) NextEvent() (*Event, error) {
	event, err := l.nextEventInternal()
	if event != nil {
		return event, nil
	}
	return nil, err
}

func NewLogFromByteReader(reader *bytes.Reader, options Options) (*Log, error) {
	return newLogFromReader(reader, options)
}

func NewLogFromFile(file *os.File, options Options) (*Log, error) {
	return newLogFromReader(file, options)
}
