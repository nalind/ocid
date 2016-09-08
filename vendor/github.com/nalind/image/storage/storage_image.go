package storage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"strings"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/nalind/image/image"
	"github.com/nalind/image/types"
	"github.com/containers/storage/pkg/archive"
	"github.com/containers/storage/storage"
)

var (
	// ErrBlobDigestMismatch is returned when PutBlob() is given a blob
	// with a digest-based name that doesn't match its contents.
	ErrBlobDigestMismatch = errors.New("blob digest mismatch")
)

type storageImage struct {
	store          storage.Store
	imageRef       *storageReference
	Tag            string            `json:"tag,omitifempty"`
	Created        time.Time         `json:"created-time,omitifempty"`
	ID             string            `json:"id"`
	BlobList       []string          `json:"blob-list,omitifempty"` // Ordered list of every blob the image has been told to handle
	Layers         map[string]string `json:"layers,omitifempty"`    // Map from names of blobs that are layers to layer IDs
	BlobData       map[string][]byte `json:"-"`                     // Map from names of blobs that aren't layers to contents, temporary
	SignatureSizes []int             `json:"signature-sizes"`       // List of sizes of each signature slice
}

type storageLayerMetadata struct {
	Compression  archive.Compression `json:"compression"`
	ExpectedSize int64               `json:"expected-size"`
}

// newImageSource sets us up to read out an image, which we assume exists.
func newImageSource(imageRef *storageReference) *storageImage {
	ref := imageRef.reference
	tag := ""
	if imageRef.id != "" {
		ref = imageRef.id
		tag = imageRef.reference
	}
	img, err := imageRef.store.GetImage(ref)
	if err != nil {
		return nil
	}
	image := storageImage{
		store:    imageRef.store,
		imageRef: imageRef,
		Tag:      tag,
		Created:  time.Now(),
		ID:       img.ID,
		BlobList: []string{},
		Layers:   make(map[string]string),
		BlobData: make(map[string][]byte),
	}
	image.loadMetadata()
	return &image
}

// newImageDestination sets us up to write a new image.
func newImageDestination(imageRef *storageReference) *storageImage {
	// We set the image's ID if the reference we got looked like one, since
	// we take that as an indication that it's going to end up with the
	// same contents as an image we already have.  If the reference looks
	// more like a name, we don't know yet if it'll be exactly the same as,
	// or be an updated version of, an image we might already have, so we
	// have to err on the side of caution and create a new image which will
	// be assigned the name as its tag.
	image := storageImage{
		store:    imageRef.store,
		imageRef: imageRef,
		Tag:      imageRef.reference,
		Created:  time.Now(),
		ID:       imageRef.id,
		BlobList: []string{},
		Layers:   make(map[string]string),
		BlobData: make(map[string][]byte),
	}
	return &image
}

func newImage(imageRef *storageReference) *storageImage {
	return newImageSource(imageRef)
}

func (s *storageImage) loadMetadata() {
	if s.ID != "" {
		image, err := s.store.GetImage(s.ID)
		if image != nil && err == nil {
			json.Unmarshal([]byte(image.Metadata), s)
		}
	}
}

func (s *storageImage) saveMetadata() {
	if s.ID != "" {
		if metadata, err := json.Marshal(s); len(metadata) != 0 && err == nil {
			istore, err := s.store.GetImageStore()
			if istore != nil && err == nil {
				istore.SetMetadata(s.ID, string(metadata))
			}
		}
	}
}

func (s *storageImage) Reference() types.ImageReference {
	s.ID = s.imageRef.ID()
	return s.imageRef
}

func (s *storageImage) Close() {
}

// SupportsSignatures returns an error if we can't expect GetSignatures() to
// return data that was previously supplied to PutSignatures().
func (s *storageImage) SupportsSignatures() error {
	return nil
}

// PutBlob is used to both store filesystem layers and binary data that is part
// of the image.
func (s *storageImage) PutBlob(stream io.Reader, digest string, expectedSize int64) (actualDigest string, actualSize int64, err error) {
	// Try to read an initial snippet of the blob.
	header := make([]byte, 10240)
	n, err := stream.Read(header)
	if err != nil && err != io.EOF {
		return "", -1, err
	}
	// Set up to read the whole blob (the initial snippet, plus the rest)
	// while digesting it with sha256.
	hasher := sha256.New()
	defragmented := io.MultiReader(bytes.NewBuffer(header[:n]), stream)
	multi := io.TeeReader(defragmented, hasher)
	if (n > 0) && archive.IsArchive(header[:n]) {
		// It's a filesystem layer.  If it's not the first one in the
		// image, we assume that the most recently added layer is its
		// parent.
		parentLayer := ""
		if len(s.BlobList) > 0 {
			for _, blob := range s.BlobList {
				if layerID, ok := s.Layers[blob]; ok {
					parentLayer = layerID
				}
			}
		}
		// Now try to figure out if the identifier we have is an ID or
		// something else, so that we can do collision detection
		// correctly.
		names := []string{digest}
		id := digest
		if matches := idRegexp.FindStringSubmatch(digest); len(matches) > 1 {
			id = matches[len(matches)-1]
			names = nil
		}
		layer, err := s.store.CreateLayer(id, parentLayer, names, "", true)
		if err != nil && err != storage.ErrDuplicateID {
			return "", -1, err
		}
		if err == storage.ErrDuplicateID {
			// There's already a layer with the same ID.  If its
			// checksum is the same as ours, then we're cool with
			// just using the already-stored copy.
			io.Copy(ioutil.Discard, multi)
			hash := hasher.Sum(nil)
			hasher = sha256.New()
			diff, _, err := s.diffLayer(id, false)
			if err != nil {
				logrus.Debugf("blob %q already present but got %v while verifying it", err)
				return "", -1, err
			}
			io.Copy(hasher, diff)
			diff.Close()
			if bytes.Compare(hash, hasher.Sum(nil)) != 0 {
				// They are not the same.  Return an error.
				logrus.Debugf("blob %q already present with different contents", id)
				return "", -1, storage.ErrDuplicateID
			}
			// They are the same.  Record that this blob is a layer.
			logrus.Debugf("blob %q already present with same contents, continuing", id)
			if layer, err = s.store.GetLayer(id); err == nil && layer != nil {
				id = layer.ID
			}
			s.Layers[digest] = id
		} else {
			// Apply the layer.
			actualSize, err = s.store.ApplyDiff(layer.ID, multi)
			if err != nil {
				s.store.DeleteLayer(layer.ID)
				return "", -1, err
			}
			// If the diff was compressed, make a note of how it
			// was compressed, so that we can compress it again if
			// we need to extract it via GetBlob().
			layerdata := storageLayerMetadata{
				Compression:  archive.DetectCompression(header[:n]),
				ExpectedSize: expectedSize,
			}
			if metadata, err := json.Marshal(layerdata); len(metadata) != 0 && err == nil {
				s.store.SetMetadata(layer.ID, string(metadata))
			}
			// Record that this blob is a layer.
			s.Layers[digest] = layer.ID
			logrus.Debugf("blob %q imported as a filesystem layer", id)
		}
		// Check that our computed sha256sum matches the digest, then
		// add it to the blob list.
		hash := hasher.Sum(nil)
		if strings.HasPrefix(digest, "sha256:") && digest != "sha256:"+hex.EncodeToString(hash[:]) {
			logrus.Debugf("layer %q digests to %q, rejecting", digest, hex.EncodeToString(hash[:]))
			return "sha256:" + hex.EncodeToString(hash[:]), -1, ErrBlobDigestMismatch
		}
		s.BlobList = append(s.BlobList, digest)
	} else {
		// It's just data.  Finish scanning it in, check that our
		// computed sha256sum matches the digest, and store it, but do
		// not add it to the blob-to-layer-ID map so that we can tell
		// that it's not a layer.
		blob, err := ioutil.ReadAll(multi)
		if err != nil && err != io.EOF {
			return "", -1, err
		}
		actualSize = int64(len(blob))
		hash := hasher.Sum(nil)
		if strings.HasPrefix(digest, "sha256:") && digest != "sha256:"+hex.EncodeToString(hash[:]) {
			logrus.Debugf("blob %q digests to %q, rejecting", digest, hex.EncodeToString(hash[:]))
			return "sha256:" + hex.EncodeToString(hash[:]), -1, ErrBlobDigestMismatch
		}
		s.BlobData[digest] = blob
		s.BlobList = append(s.BlobList, digest)
		logrus.Debugf("blob %q imported as opaque data", digest)
	}
	return digest, actualSize, nil
}

func (s *storageImage) Commit() error {
	if s.ID != "" {
		// We started with an image ID, or we've already registered
		// this one and gotten one, so no need to do anything more.
		if img, err := s.store.GetImage(s.ID); img != nil && err == nil {
			return nil
		}
	}
	lastLayer := ""
	if len(s.BlobList) > 0 {
		for _, blob := range s.BlobList {
			if layerID, ok := s.Layers[blob]; ok {
				lastLayer = layerID
			}
		}
	}
	img, err := s.store.CreateImage(s.ID, nil, lastLayer, "")
	if err != nil {
		return err
	}
	logrus.Debugf("created new image ID %q", img.ID)
	if s.Tag != "" {
		// We started with an image name rather than an ID, so move the
		// name to this image.
		if err := s.store.SetNames(img.ID, []string{s.Tag}); err != nil {
			return err
		}
		logrus.Debugf("set name of image %q to %q", img.ID, s.Tag)
	}
	// Save the blob data to disk, and drop the contents from memory.
	keys := []string{}
	for k, v := range s.BlobData {
		if err := s.store.SetImageBigData(img.ID, k, v); err != nil {
			return err
		}
		keys = append(keys, k)
	}
	for _, key := range keys {
		delete(s.BlobData, key)
	}
	s.ID = img.ID
	return nil
}

func (s *storageImage) PutManifest(manifest []byte) error {
	if err := s.Commit(); err != nil {
		return err
	}
	defer s.saveMetadata()
	return s.store.SetImageBigData(s.ID, "manifest", manifest)
}

func (s *storageImage) PutSignatures(signatures [][]byte) error {
	if err := s.Commit(); err != nil {
		return err
	}
	sizes := []int{}
	sigblob := []byte{}
	for _, sig := range signatures {
		sizes = append(sizes, len(sig))
		newblob := make([]byte, len(sigblob)+len(sig))
		copy(newblob, sigblob)
		copy(newblob[len(sigblob):], sig)
		sigblob = newblob
	}
	s.SignatureSizes = sizes
	defer s.saveMetadata()
	return s.store.SetImageBigData(s.ID, "signatures", sigblob)
}

func (s *storageImage) SupportedManifestMIMETypes() []string {
	return nil
}

func (s *storageImage) GetBlob(digest string) (rc io.ReadCloser, n int64, err error) {
	if blob, ok := s.BlobData[digest]; ok {
		r := bytes.NewReader(blob)
		return ioutil.NopCloser(r), r.Size(), nil
	}
	if _, ok := s.Layers[digest]; !ok {
		b, err := s.store.GetImageBigData(s.ID, digest)
		if err != nil {
			return nil, -1, err
		}
		r := bytes.NewReader(b)
		logrus.Debugf("exporting opaque data as blob %q", digest)
		return ioutil.NopCloser(r), r.Size(), nil
	}
	logrus.Debugf("exporting filesystem layer as blob %q", digest)
	return s.diffLayer(s.Layers[digest], true)
}

func (s *storageImage) diffLayer(layerID string, computeSize bool) (rc io.ReadCloser, n int64, err error) {
	layer, err := s.store.GetLayer(layerID)
	if err != nil {
		return nil, -1, err
	}
	layerMeta := storageLayerMetadata{
		Compression:  archive.Uncompressed,
		ExpectedSize: -1,
	}
	if layer.Metadata != "" {
		json.Unmarshal([]byte(layer.Metadata), &layerMeta)
	}
	if computeSize {
		if layerMeta.ExpectedSize == -1 {
			n, err = s.store.DiffSize("", layer.ID)
			if err != nil {
				return nil, -1, err
			}
		} else {
			n = layerMeta.ExpectedSize
		}
	} else {
		n = -1
	}
	diff, err := s.store.Diff("", layer.ID)
	if err != nil {
		return nil, -1, err
	}
	if layerMeta.Compression == archive.Uncompressed {
		return diff, n, nil
	}
	preader, pwriter := io.Pipe()
	compressor, err := archive.CompressStream(pwriter, layerMeta.Compression)
	if err != nil {
		return nil, -1, err
	}
	go func() {
		io.Copy(compressor, diff)
		diff.Close()
		compressor.Close()
		pwriter.Close()
	}()
	return preader, n, err
}

func (s *storageImage) GetManifest() (manifest []byte, MIMEType string, err error) {
	manifest, err = s.store.GetImageBigData(s.ID, "manifest")
	return manifest, "", err
}

func (s *storageImage) GetSignatures() (signatures [][]byte, err error) {
	var offset int
	s.loadMetadata()
	signature, err := s.store.GetImageBigData(s.ID, "signatures")
	if err != nil {
		return nil, err
	}
	sigslice := [][]byte{}
	for _, length := range s.SignatureSizes {
		sigslice = append(sigslice, signature[offset:offset+length])
		offset += length
	}
	if offset != len(signature) {
		return nil, fmt.Errorf("signatures data contained %d extra bytes", len(signatures)-offset)
	}
	return sigslice, nil
}

func (s *storageImage) DeleteImage() error {
	if s.ID != "" {
		if _, err := s.store.DeleteImage(s.ID, true); err != nil {
			return err
		}
		s.ID = ""
	}
	return nil
}

func (s *storageImage) Manifest() (manifest []byte, MIMEType string, err error) {
	return s.GetManifest()
}

func (s *storageImage) Signatures() (signatures [][]byte, err error) {
	return s.GetSignatures()
}

func (s *storageImage) BlobDigests() (digests []string, err error) {
	return image.FromSource(s).BlobDigests()
}

func (s *storageImage) Inspect() (info *types.ImageInspectInfo, err error) {
	return image.FromSource(s).Inspect()
}
