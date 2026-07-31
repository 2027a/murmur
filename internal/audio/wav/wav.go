// Package wav reads and writes the PCM format used by Murmur.
package wav

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"git.2027a.net/mathieu/murmur/internal/errs"
)

const (
	SampleRate    = 16000
	Channels      = 1
	BitsPerSample = 16
	BlockAlign    = Channels * BitsPerSample / 8
	ByteRate      = SampleRate * BlockAlign
	headerSize    = 44
)

type File struct {
	path       string
	dataOffset int64
	dataSize   int64
}

func Open(path string) (File, error) {
	handle, err := os.Open(path)
	if err != nil {
		return File{}, audioError("impossible d'ouvrir le fichier WAV", err)
	}
	defer handle.Close()

	info, err := handle.Stat()
	if err != nil {
		return File{}, audioError("impossible d'inspecter le fichier WAV", err)
	}
	if info.Size() < 12 {
		return File{}, audioError("fichier WAV tronqué", io.ErrUnexpectedEOF)
	}

	var riff [12]byte
	if _, err := io.ReadFull(handle, riff[:]); err != nil {
		return File{}, audioError("en-tête WAV tronqué", err)
	}
	if string(riff[0:4]) != "RIFF" || string(riff[8:12]) != "WAVE" {
		return File{}, audioError("conteneur WAV RIFF invalide", nil)
	}
	riffEnd := int64(binary.LittleEndian.Uint32(riff[4:8])) + 8
	if riffEnd < 12 || riffEnd > info.Size() {
		return File{}, audioError("taille RIFF invalide", io.ErrUnexpectedEOF)
	}

	var (
		formatSeen bool
		dataSeen   bool
		dataOffset int64
		dataSize   int64
	)
	for offset := int64(12); offset < riffEnd; {
		if riffEnd-offset < 8 {
			return File{}, audioError("en-tête de chunk WAV tronqué", io.ErrUnexpectedEOF)
		}
		var chunk [8]byte
		if _, err := handle.ReadAt(chunk[:], offset); err != nil {
			return File{}, audioError("chunk WAV tronqué", err)
		}
		size := int64(binary.LittleEndian.Uint32(chunk[4:8]))
		payload := offset + 8
		end := payload + size
		if end < payload || end > riffEnd {
			return File{}, audioError("taille de chunk WAV invalide", io.ErrUnexpectedEOF)
		}

		switch string(chunk[0:4]) {
		case "fmt ":
			if formatSeen {
				return File{}, audioError("plusieurs chunks fmt dans le WAV", nil)
			}
			if size < 16 {
				return File{}, audioError("chunk fmt WAV trop court", nil)
			}
			var format [16]byte
			if _, err := handle.ReadAt(format[:], payload); err != nil {
				return File{}, audioError("chunk fmt WAV tronqué", err)
			}
			if err := validateFormat(format[:]); err != nil {
				return File{}, err
			}
			formatSeen = true
		case "data":
			if dataSeen {
				return File{}, audioError("plusieurs chunks data dans le WAV", nil)
			}
			dataSeen = true
			dataOffset = payload
			dataSize = size
		}

		offset = end
		if size&1 != 0 {
			offset++
			if offset > riffEnd {
				return File{}, audioError("padding de chunk WAV manquant", io.ErrUnexpectedEOF)
			}
		}
	}
	if !formatSeen {
		return File{}, audioError("chunk fmt WAV absent", nil)
	}
	if !dataSeen {
		return File{}, audioError("chunk data WAV absent", nil)
	}
	if dataSize%BlockAlign != 0 {
		return File{}, audioError("données WAV non alignées sur un échantillon", nil)
	}
	return File{path: path, dataOffset: dataOffset, dataSize: dataSize}, nil
}

func (f File) Path() string { return f.path }

func (f File) DataSize() int64 { return f.dataSize }

func (f File) Samples() int64 { return f.dataSize / BlockAlign }

func (f File) Duration() time.Duration {
	return time.Duration(f.Samples()) * time.Second / SampleRate
}

func (f File) OpenData() (*io.SectionReader, *os.File, error) {
	handle, err := os.Open(f.path)
	if err != nil {
		return nil, nil, audioError("impossible d'ouvrir les données WAV", err)
	}
	return io.NewSectionReader(handle, f.dataOffset, f.dataSize), handle, nil
}

// HasAudioData accepts an unfinished RIFF size but requires a valid fmt chunk
// and at least one complete sample physically present after the data header.
func HasAudioData(path string) (bool, error) {
	handle, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, audioError("impossible de sonder le fichier WAV", err)
	}
	defer handle.Close()
	info, err := handle.Stat()
	if err != nil {
		return false, audioError("impossible de sonder le fichier WAV", err)
	}
	if info.Size() < 12 {
		return false, nil
	}
	var riff [12]byte
	if _, err := handle.ReadAt(riff[:], 0); err != nil {
		return false, nil
	}
	if string(riff[0:4]) != "RIFF" || string(riff[8:12]) != "WAVE" {
		return false, audioError("conteneur WAV RIFF invalide", nil)
	}

	formatSeen := false
	for offset := int64(12); offset+8 <= info.Size(); {
		var chunk [8]byte
		if _, err := handle.ReadAt(chunk[:], offset); err != nil {
			return false, nil
		}
		size := int64(binary.LittleEndian.Uint32(chunk[4:8]))
		payload := offset + 8
		switch string(chunk[0:4]) {
		case "fmt ":
			if size < 16 || payload+16 > info.Size() {
				return false, nil
			}
			var format [16]byte
			if _, err := handle.ReadAt(format[:], payload); err != nil {
				return false, nil
			}
			if err := validateFormat(format[:]); err != nil {
				return false, err
			}
			formatSeen = true
		case "data":
			return formatSeen && info.Size() >= payload+BlockAlign, nil
		}
		end := payload + size
		if end < payload || end > info.Size() {
			return false, nil
		}
		offset = end + size%2
	}
	return false, nil
}

// Snapshot returns the complete PCM samples physically present in a growing
// WAV. RIFF and data sizes are not trusted until the writer has finalized them.
func Snapshot(path string) (File, bool, error) {
	handle, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return File{}, false, nil
		}
		return File{}, false, audioError("impossible d'ouvrir le snapshot WAV", err)
	}
	defer handle.Close()
	info, err := handle.Stat()
	if err != nil {
		return File{}, false, audioError("impossible d'inspecter le snapshot WAV", err)
	}
	if info.Size() < 12 {
		return File{}, false, nil
	}
	var riff [12]byte
	if _, err := handle.ReadAt(riff[:], 0); err != nil {
		return File{}, false, nil
	}
	if string(riff[0:4]) != "RIFF" || string(riff[8:12]) != "WAVE" {
		return File{}, false, audioError("conteneur WAV RIFF invalide", nil)
	}
	formatSeen := false
	for offset := int64(12); offset+8 <= info.Size(); {
		var header [8]byte
		if _, err := handle.ReadAt(header[:], offset); err != nil {
			return File{}, false, nil
		}
		size := int64(binary.LittleEndian.Uint32(header[4:8]))
		payload := offset + 8
		switch string(header[0:4]) {
		case "fmt ":
			if size < 16 || payload+16 > info.Size() {
				return File{}, false, nil
			}
			var format [16]byte
			if _, err := handle.ReadAt(format[:], payload); err != nil {
				return File{}, false, nil
			}
			if err := validateFormat(format[:]); err != nil {
				return File{}, false, err
			}
			formatSeen = true
		case "data":
			if !formatSeen {
				return File{}, false, audioError("chunk data avant le format WAV", nil)
			}
			physical := info.Size() - payload
			physical -= physical % BlockAlign
			return File{path: path, dataOffset: payload, dataSize: physical}, true, nil
		}
		end := payload + size
		if end < payload || end > info.Size() {
			return File{}, false, nil
		}
		offset = end + size%2
	}
	return File{}, false, nil
}

func CreateGrowing(path string) error {
	if err := os.WriteFile(path, canonicalHeader(0), 0o600); err != nil {
		return audioError("impossible de créer le WAV en croissance", err)
	}
	return nil
}

func AppendPCM(path string, data []byte) error {
	if len(data)%BlockAlign != 0 {
		return audioError("données PCM non alignées", nil)
	}
	handle, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return audioError("impossible d'ouvrir le WAV en croissance", err)
	}
	_, writeErr := handle.Write(data)
	closeErr := handle.Close()
	if writeErr != nil || closeErr != nil {
		return audioError("impossible d'ajouter les données PCM", errors.Join(writeErr, closeErr))
	}
	return nil
}

func FinalizeGrowing(path string) (File, error) {
	snapshot, ok, err := Snapshot(path)
	if err != nil {
		return File{}, err
	}
	if !ok || snapshot.dataSize > int64(^uint32(0))-36 {
		return File{}, audioError("WAV en croissance non finalisable", nil)
	}
	handle, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return File{}, audioError("impossible d'ouvrir le WAV à finaliser", err)
	}
	header := canonicalHeader(uint32(snapshot.dataSize))
	_, writeErr := handle.WriteAt(header, 0)
	syncErr := handle.Sync()
	closeErr := handle.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return File{}, audioError("impossible de finaliser le WAV en croissance", errors.Join(writeErr, syncErr, closeErr))
	}
	return Open(path)
}

func (f File) Slice(outputDir string, start, samples int64) (File, error) {
	if start < 0 || samples <= 0 || start > f.Samples() || samples > f.Samples()-start {
		return File{}, audioError("plage d'échantillons WAV invalide", nil)
	}
	if outputDir == "" {
		outputDir = filepath.Dir(f.path)
	}
	source, err := os.Open(f.path)
	if err != nil {
		return File{}, audioError("impossible d'ouvrir le WAV à segmenter", err)
	}
	defer source.Close()
	segment, _, err := writeSegment(outputDir, source, f.dataOffset+start*BlockAlign, samples*BlockAlign)
	return segment, err
}

func (f File) Split(outputDir string, d, minTail time.Duration) ([]File, error) {
	if d <= 0 {
		return nil, audioError("durée de segment invalide", nil)
	}
	if minTail < 0 {
		return nil, audioError("durée minimale de fin invalide", nil)
	}
	maxSamples := durationSamples(d)
	if maxSamples < 1 {
		return nil, audioError("durée de segment inférieure à un échantillon", nil)
	}
	if f.Samples() <= maxSamples {
		return []File{f}, nil
	}

	sizes := splitSizes(f.Samples(), maxSamples, durationSamples(minTail))
	if outputDir == "" {
		outputDir = filepath.Dir(f.path)
	}
	if err := os.MkdirAll(outputDir, 0o700); err != nil {
		return nil, audioError("impossible de créer le répertoire des segments", err)
	}
	source, err := os.Open(f.path)
	if err != nil {
		return nil, audioError("impossible d'ouvrir le WAV à découper", err)
	}
	defer source.Close()

	created := make([]string, 0, len(sizes))
	result := make([]File, 0, len(sizes))
	rollback := func() {
		for _, path := range created {
			_ = os.Remove(path)
		}
	}
	offset := f.dataOffset
	for _, samples := range sizes {
		dataSize := samples * BlockAlign
		segment, path, err := writeSegment(outputDir, source, offset, dataSize)
		if err != nil {
			rollback()
			return nil, err
		}
		created = append(created, path)
		result = append(result, segment)
		offset += dataSize
	}
	return result, nil
}

// Concat writes the sample data of files in order into one canonical WAV.
func Concat(outputDir string, files ...File) (File, error) {
	if len(files) == 0 {
		return File{}, audioError("aucun segment WAV à concaténer", nil)
	}
	var dataSize int64
	for _, file := range files {
		if file.path == "" || file.dataSize > int64(^uint32(0))-36-dataSize {
			return File{}, audioError("concaténation WAV trop volumineuse", nil)
		}
		dataSize += file.dataSize
	}
	if outputDir == "" {
		outputDir = filepath.Dir(files[0].path)
	}
	if err := os.MkdirAll(outputDir, 0o700); err != nil {
		return File{}, audioError("impossible de créer le répertoire WAV", err)
	}
	handle, err := os.CreateTemp(outputDir, "murmur-concat-*.wav")
	if err != nil {
		return File{}, audioError("impossible de créer le WAV concaténé", err)
	}
	path := handle.Name()
	ok := false
	defer func() {
		_ = handle.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()
	if _, err := handle.Write(canonicalHeader(uint32(dataSize))); err != nil {
		return File{}, audioError("impossible d'écrire l'en-tête WAV concaténé", err)
	}
	for _, file := range files {
		reader, source, err := file.OpenData()
		if err != nil {
			return File{}, err
		}
		_, copyErr := io.Copy(handle, reader)
		closeErr := source.Close()
		if copyErr != nil || closeErr != nil {
			return File{}, audioError("impossible de concaténer les données WAV", errors.Join(copyErr, closeErr))
		}
	}
	if err := handle.Sync(); err != nil {
		return File{}, audioError("impossible de synchroniser le WAV concaténé", err)
	}
	if err := handle.Close(); err != nil {
		return File{}, audioError("impossible de fermer le WAV concaténé", err)
	}
	result, err := Open(path)
	if err != nil {
		return File{}, err
	}
	ok = true
	return result, nil
}

func splitSizes(total, maximum, minimumTail int64) []int64 {
	var sizes []int64
	for total > maximum {
		sizes = append(sizes, maximum)
		total -= maximum
	}
	if total > 0 {
		if len(sizes) > 0 && total < minimumTail {
			sizes[len(sizes)-1] += total
		} else {
			sizes = append(sizes, total)
		}
	}
	return sizes
}

func durationSamples(d time.Duration) int64 {
	return int64(d) * SampleRate / int64(time.Second)
}

func writeSegment(dir string, source *os.File, offset, dataSize int64) (File, string, error) {
	if dataSize > int64(^uint32(0))-36 {
		return File{}, "", audioError("segment WAV trop volumineux", nil)
	}
	handle, err := os.CreateTemp(dir, "murmur-chunk-*.wav")
	if err != nil {
		return File{}, "", audioError("impossible de créer un segment WAV", err)
	}
	path := handle.Name()
	ok := false
	defer func() {
		_ = handle.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()

	if _, err := handle.Write(canonicalHeader(uint32(dataSize))); err != nil {
		return File{}, "", audioError("impossible d'écrire l'en-tête du segment WAV", err)
	}
	reader := io.NewSectionReader(source, offset, dataSize)
	if _, err := io.CopyN(handle, reader, dataSize); err != nil {
		return File{}, "", audioError("impossible d'écrire les données du segment WAV", err)
	}
	if err := handle.Sync(); err != nil {
		return File{}, "", audioError("impossible de synchroniser le segment WAV", err)
	}
	if err := handle.Close(); err != nil {
		return File{}, "", audioError("impossible de fermer le segment WAV", err)
	}
	segment, err := Open(path)
	if err != nil {
		return File{}, "", err
	}
	ok = true
	return segment, path, nil
}

func canonicalHeader(dataSize uint32) []byte {
	header := make([]byte, headerSize)
	copy(header[0:4], "RIFF")
	binary.LittleEndian.PutUint32(header[4:8], dataSize+36)
	copy(header[8:12], "WAVE")
	copy(header[12:16], "fmt ")
	binary.LittleEndian.PutUint32(header[16:20], 16)
	binary.LittleEndian.PutUint16(header[20:22], 1)
	binary.LittleEndian.PutUint16(header[22:24], Channels)
	binary.LittleEndian.PutUint32(header[24:28], SampleRate)
	binary.LittleEndian.PutUint32(header[28:32], ByteRate)
	binary.LittleEndian.PutUint16(header[32:34], BlockAlign)
	binary.LittleEndian.PutUint16(header[34:36], BitsPerSample)
	copy(header[36:40], "data")
	binary.LittleEndian.PutUint32(header[40:44], dataSize)
	return header
}

func validateFormat(format []byte) error {
	encoding := binary.LittleEndian.Uint16(format[0:2])
	channels := binary.LittleEndian.Uint16(format[2:4])
	rate := binary.LittleEndian.Uint32(format[4:8])
	byteRate := binary.LittleEndian.Uint32(format[8:12])
	align := binary.LittleEndian.Uint16(format[12:14])
	bits := binary.LittleEndian.Uint16(format[14:16])
	if encoding != 1 || channels != Channels || rate != SampleRate || bits != BitsPerSample || align != BlockAlign || byteRate != ByteRate {
		return audioError(fmt.Sprintf("format WAV attendu: PCM s16 mono %d Hz", SampleRate), nil)
	}
	return nil
}

func audioError(message string, cause error) error {
	message = strings.TrimSpace(message)
	if cause == nil {
		return errs.New(errs.AudioFailed, "audio/wav", message)
	}
	return errs.Wrap(errs.AudioFailed, "audio/wav", message, cause)
}
