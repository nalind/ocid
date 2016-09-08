package server

import (
	"errors"

	pb "github.com/kubernetes/kubernetes/pkg/kubelet/api/v1alpha1/runtime"
	"github.com/nalind/image/image"
	"github.com/nalind/image/transports"
	"golang.org/x/net/context"
)

// ListImages lists existing images.
func (s *Server) ListImages(ctx context.Context, req *pb.ListImagesRequest) (*pb.ListImagesResponse, error) {
	images, err := s.storage.Images()
	if err != nil {
		return nil, err
	}
	resp := pb.ListImagesResponse{}
	for _, image := range images {
		idCopy := image.ID
		resp.Images = append(resp.Images, &pb.Image{
			Id:       &idCopy,
			RepoTags: image.Names,
		})
	}
	return &resp, nil
}

// ImageStatus returns the status of the image.
func (s *Server) ImageStatus(ctx context.Context, req *pb.ImageStatusRequest) (*pb.ImageStatusResponse, error) {
	image, err := s.storage.GetImage(*(req.Image.Image))
	if err != nil {
		return nil, err
	}
	resp := pb.ImageStatusResponse{}
	idCopy := image.ID
	resp.Image = &pb.Image{
		Id:       &idCopy,
		RepoTags: image.Names,
	}
	return &resp, nil
}

// PullImage pulls a image with authentication config.
func (s *Server) PullImage(ctx context.Context, req *pb.PullImageRequest) (*pb.PullImageResponse, error) {
	img := req.GetImage().GetImage()
	if img == "" {
		return nil, errors.New("got empty imagespec name")
	}

	// TODO(runcom): deal with AuthConfig in req.GetAuth()
	tr, err := transports.ParseImageName(img)
	if err != nil {
		return nil, err
	}

	// TODO(runcom): figure out the ImageContext story in containers/image instead of passing ("", true)
	src, err := tr.NewImageSource(s.imageContext, nil)
	if err != nil {
		return nil, err
	}
	i := image.FromSource(src)
	blobs, err := i.BlobDigests()
	if err != nil {
		return nil, err
	}

	dr, err := transports.ParseImageName("oci-storage:" + tr.StringWithinTransport())
	if err != nil {
		return nil, err
	}
	dest, err := dr.NewImageDestination(s.imageContext)
	if err != nil {
		return nil, err
	}
	defer dest.Close()

	// copy blobs (layer + config for docker v2s2, layers only for docker v2s1 [the config is in the manifest])
	for _, b := range blobs {
		// TODO(runcom,nalin): we need do-then-commit to later purge on error
		r, s, err := src.GetBlob(b)
		if err != nil {
			return nil, err
		}
		if _, _, err := dest.PutBlob(r, b, s); err != nil {
			r.Close()
			return nil, err
		}
		r.Close()
	}

	// make the destination image persistent
	err = dest.Commit()
	if err != nil {
		return nil, err
	}

	// copy manifest
	m, _, err := i.Manifest()
	if err != nil {
		return nil, err
	}
	if err := dest.PutManifest(m); err != nil {
		return nil, err
	}

	// copy signatures, if we have any
	signatures, err := src.GetSignatures()
	if err != nil {
		return nil, err
	}
	if len(signatures) > 0 {
		if err = dest.PutSignatures(signatures); err != nil {
			return nil, err
		}
	}

	return &pb.PullImageResponse{}, nil
}

// RemoveImage removes the image.
func (s *Server) RemoveImage(ctx context.Context, req *pb.RemoveImageRequest) (*pb.RemoveImageResponse, error) {
	_, err := s.storage.DeleteImage(*(req.Image.Image), true)
	return nil, err
}
