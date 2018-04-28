package flasher

import (
	"bytes"
	"context"
	"crypto/md5"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hashicorp/packer/helper/config"
	"github.com/hashicorp/packer/packer"
	"github.com/hashicorp/packer/template/interpolate"
	"github.com/solo-io/packer-builder-arm-image/pkg/image"
	imageutils "github.com/solo-io/packer-builder-arm-image/pkg/image/utils"
	"github.com/solo-io/packer-builder-arm-image/pkg/utils"
)

const BlockSize = 512

type FlashConfig struct {
	Image          string
	Device         string
	NotInteractive bool
	Verify         bool
}

type Flasher interface {
	Flash() error
	// Verify() error
}

type flasher struct {
	config      FlashConfig
	ui          packer.Ui
	imageOpener image.ImageOpener
}

type FlashResult struct {
	Sum          []byte
	BytesWritten uint64
}

var newHasher = md5.New

func NewFlasher(ui packer.Ui, cfg FlashConfig) Flasher {
	return &flasher{config: cfg, ui: ui, imageOpener: image.NewImageOpener(ui)}
}

func (f *flasher) Flash() error {

	imageToFlash, err := f.getSource()
	if err != nil {
		return err
	}
	defer imageToFlash.Close()

	dev, err := f.getDevice()
	if err != nil {
		return err
	}

	f.ui.Say(fmt.Sprintf("Going to flash to %s.", dev.Device))
	if !f.config.NotInteractive {
		answer, err := f.ui.Ask("Are you sure?")
		if err != nil {
			return err
		}
		answer = strings.TrimSpace(strings.ToLower(answer))
		if !strings.HasPrefix("yes", answer) {
			return errors.New("canceled by user")
		}
	}

	err = f.unmount(dev)
	if err != nil {
		return err
	}
	res, err := f.flash(imageToFlash, dev)
	if err != nil {
		return err
	}

	syscall.Sync()

	if len(res.Sum) != 0 {
		f.verify(*res, dev)
	}

	return nil
}

func (f *flasher) getSource() (image.Image, error) {
	if len(f.config.Image) != 0 {
		return f.imageOpener.Open(f.config.Image)
	}

	potentials := imageutils.GetImageFilesInCurrentDir()
	if len(potentials) == 0 {
		return nil, errors.New("can't find source")
	}
	var chosen string
	var err error
	if f.config.NotInteractive {
		// chose the most recent one
		chosen, err = f.getMostRecent(potentials)
	} else {
		// ask the user
		chosen, err = f.Choose(potentials)
	}

	if err != nil {
		return nil, err
	}
	f.ui.Say("using image " + chosen)
	return f.imageOpener.Open(chosen)
}

func (f *flasher) getMostRecent(files []string) (string, error) {

	var maxModified time.Time
	max := ""

	for _, f := range files {
		if fi, err := os.Stat(f); err != nil {
			return "", err
		} else {
			if max == "" {
				max = f
				maxModified = fi.ModTime()
			} else if maxModified.Before(fi.ModTime()) {
				max = f
				maxModified = fi.ModTime()
			}
		}
	}

	return max, nil
}

func (f *flasher) Choose(files []string) (string, error) {
	images := ""
	for i, f := range files {
		images += fmt.Sprintf("%d. %s\n", i+1, f)
	}

	answer, err := f.ui.Ask(images + "Which image should we use (type number)?")
	if err != nil {
		return "", err
	}

	index, err := strconv.Atoi(answer)
	if err != nil {
		return "", err
	}
	if (index <= 0) || (index > len(files)) {
		return "", errors.New("invalid image chosen")
	}
	return files[index-1], nil
}

func (f *flasher) Configure(cfgs ...interface{}) error {
	err := config.Decode(&f.config, &config.DecodeOpts{
		Interpolate:       true,
		InterpolateFilter: &interpolate.RenderFilter{},
	}, cfgs...)
	if err != nil {
		return err
	}
	return nil
}
func (f *flasher) unmount(device *utils.Device) error {
	for _, mntpnt := range device.Mountpoints {
		f.ui.Say("unmounting " + mntpnt)
		err := exec.Command("umount", mntpnt).Run()
		if err != nil {
			return err
		}
	}
	return nil
}

type WriterSeeker interface {
	io.Seeker
	io.Writer
}

func (f *flasher) flash(input image.Image, device *utils.Device) (*FlashResult, error) {

	defer input.Close()
	output, err := os.OpenFile(device.Device, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	defer output.Close()

	// TODO : support signals
	ctx := context.Background()

	var checksummer hash.Hash
	var outputWriter io.Writer = output

	if f.config.Verify {
		checksummer = newHasher()
		outputWriter = io.MultiWriter(output, checksummer)
	}

	totaldata, err := utils.CopyWithProgress(ctx, f.ui, outputWriter, input)

	res := FlashResult{BytesWritten: uint64(totaldata)}
	if checksummer != nil {
		res.Sum = checksummer.Sum(nil)
	}

	return &res, nil
}

func (f *flasher) verify(res FlashResult, dev *utils.Device) error {

	input, err := os.OpenFile(dev.Device, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer input.Close()
	checksummer := newHasher()

	limitedInput := &io.LimitedReader{
		R: input,
		N: int64(res.BytesWritten),
	}

	// TODO : support signals
	ctx := context.Background()
	_, err = utils.CopyWithProgress(ctx, f.ui, checksummer, limitedInput)

	if err != nil {
		return err
	}

	if !bytes.Equal(checksummer.Sum(nil), res.Sum) {
		return errors.New("checksums different - validation failed")
	}

	return nil
}

func (f *flasher) getDevice() (*utils.Device, error) {

	detachables, err := utils.GetDetachableDevices()
	if err != nil {
		return nil, err
	}
	if len(detachables) == 0 {
		return nil, errors.New("no devices")
	}

	if len(f.config.Device) != 0 {
		for _, d := range detachables {
			if d.Device == f.config.Device {
				return &d, nil
			}
		}
		return nil, errors.New("configured device not found")
	}

	if f.config.NotInteractive {
		if len(detachables) != 1 {
			return nil, errors.New("ambiguous device")

		}
		return &detachables[0], nil
	}

	question := "Which device should we choose?:\n"
	for i, d := range detachables {
		question += fmt.Sprintf("%d. %s (%s)\n", i+1, d.Device, d.Name)
	}
	answer, err := f.ui.Ask(question)
	if err != nil {
		return nil, err
	}
	i, err := strconv.Atoi(answer)
	if err != nil {
		return nil, err
	}
	i = i - 1
	if i < 0 || i > len(detachables) {
		return nil, errors.New("invalid selection")
	}
	return &detachables[i], nil

}