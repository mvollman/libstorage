// +build !libstorage_storage_driver libstorage_storage_driver_dobs

package storage

import (
	"strconv"
	"time"

	gofig "github.com/akutz/gofig/types"
	"github.com/akutz/goof"

	"github.com/digitalocean/godo"

	"github.com/codedellemc/libstorage/api/context"
	"github.com/codedellemc/libstorage/api/registry"
	"github.com/codedellemc/libstorage/api/types"

	do "github.com/codedellemc/libstorage/drivers/storage/dobs"
)

const (
	minSizeGiB = 1
)

type driver struct {
	name   string
	config gofig.Config
	client *godo.Client
}

func init() {
	registry.RegisterStorageDriver(do.Name, newDriver)
}

func newDriver() types.StorageDriver {
	return &driver{name: do.Name}
}

func (d *driver) Name() string {
	return do.Name
}

func (d *driver) Init(
	ctx types.Context,
	config gofig.Config) error {

	d.config = config
	token := d.config.GetString(do.ConfigDOToken)

	fields := map[string]interface{}{
		"token": token,
	}

	if token == "" {
		fields["token"] = ""
	} else {
		fields["token"] = "******"
	}

	fields["region"] = d.config.GetString(do.ConfigDORegion)

	client, err := Client(token)
	if err != nil {
		return err
	}
	d.client = client

	ctx.WithFields(fields).Info("storage driver initialized")

	return nil
}

func (d *driver) Type(ctx types.Context) (types.StorageType, error) {
	return types.Block, nil
}

// DigitalOcean volumes are are found using device-by-id, ex:
// /dev/disk/by-id/scsi-0DO_Volume_volume-nyc1-01 See
// https://www.digitalocean.com/community/tutorials/how-to-use-block-storage-on-digitalocean#preparing-volumes-for-use-in-linux
func (d *driver) NextDeviceInfo(
	ctx types.Context) (*types.NextDeviceInfo, error) {

	return nil, nil
}

func (d *driver) InstanceInspect(
	ctx types.Context,
	opts types.Store) (*types.Instance, error) {

	iid := context.MustInstanceID(ctx)
	return &types.Instance{
		InstanceID:   iid,
		Region:       iid.Fields[do.InstanceIDFieldRegion],
		Name:         iid.Fields[do.InstanceIDFieldName],
		ProviderName: iid.Driver,
	}, nil
}

func (d *driver) Volumes(
	ctx types.Context,
	opts *types.VolumesOpts) ([]*types.Volume, error) {

	doVolumes, _, err := d.client.Storage.ListVolumes(ctx, nil)
	if err != nil {
		return nil, err
	}

	var volumes []*types.Volume
	for _, vol := range doVolumes {
		volume, err := d.toTypesVolume(ctx, &vol, opts.Attachments)
		if err != nil {
			return nil, goof.New("error converting to types.Volume")
		}
		volumes = append(volumes, volume)
	}

	return volumes, nil
}

func (d *driver) VolumeInspect(
	ctx types.Context,
	volumeID string,
	opts *types.VolumeInspectOpts) (*types.Volume, error) {

	doVolume, _, err := d.client.Storage.GetVolume(ctx, volumeID)
	if err != nil {
		return nil, err
	}

	volume, err := d.toTypesVolume(ctx, doVolume, opts.Attachments)
	if err != nil {
		return nil, goof.New("error converting to types.Volume")
	}
	return volume, nil
}

func (d *driver) VolumeCreate(
	ctx types.Context,
	name string,
	opts *types.VolumeCreateOpts) (*types.Volume, error) {

	fields := map[string]interface{}{
		"volumeName": name,
	}

	if opts.AvailabilityZone == nil || *opts.AvailabilityZone == "" {
		instance, err := d.InstanceInspect(ctx, nil)
		if err != nil {
			return nil, err
		}
		opts.AvailabilityZone = &instance.Region
	}

	if opts.Size == nil {
		size := int64(minSizeGiB)
		opts.Size = &size
	}

	fields["size"] = *opts.Size

	if *opts.Size < minSizeGiB {
		fields["minSize"] = minSizeGiB
		return nil, goof.WithFields(fields, "volume size too small")
	}

	volumeReq := &godo.VolumeCreateRequest{
		Region:        *opts.AvailabilityZone,
		Name:          name,
		SizeGigaBytes: *opts.Size,
	}

	volume, _, err := d.client.Storage.CreateVolume(ctx, volumeReq)
	if err != nil {
		return nil, err
	}

	return d.VolumeInspect(ctx, volume.ID,
		&types.VolumeInspectOpts{
			Attachments: types.VolAttReqTrue,
		},
	)
}

func (d *driver) VolumeCreateFromSnapshot(
	ctx types.Context, snapshotID string, volumeName string,
	opts *types.VolumeCreateOpts) (*types.Volume, error) {
	return nil, types.ErrNotImplemented
}

func (d *driver) VolumeCopy(
	ctx types.Context, volumeID string, volumeName string,
	opts types.Store) (*types.Volume, error) {
	return nil, types.ErrNotImplemented
}

func (d *driver) VolumeSnapshot(
	ctx types.Context, volumeID string, snapshotName string,
	opts types.Store) (*types.Snapshot, error) {
	return nil, types.ErrNotImplemented
}

func (d *driver) VolumeRemove(
	ctx types.Context,
	volumeID string,
	opts *types.VolumeRemoveOpts) error {

	volume, _, err := d.client.Storage.GetVolume(ctx, volumeID)
	if err != nil {
		return err
	}

	if len(volume.DropletIDs) > 0 {
		if !opts.Force {
			return goof.New("volume currently attached")
		}

		err = d.volumeDetach(ctx, volume)
		if err != nil {
			return err
		}
	}

	_, err = d.client.Storage.DeleteVolume(ctx, volumeID)
	if err != nil {
		return err
	}

	return nil
}

func (d *driver) volumeDetach(
	ctx types.Context,
	volume *godo.Volume) error {

	for _, dropletID := range volume.DropletIDs {
		action, _, err := d.client.StorageActions.DetachByDropletID(
			ctx, volume.ID, dropletID)
		if err != nil {
			return err
		}

		err = d.waitForAction(ctx, volume.ID, action)
		if err != nil {
			return err
		}
	}
	return nil
}

func (d *driver) VolumeAttach(
	ctx types.Context, volumeID string,
	opts *types.VolumeAttachOpts) (*types.Volume, string, error) {
	vol, _, err := d.client.Storage.GetVolume(ctx, volumeID)
	if err != nil {
		return nil, "", goof.WithError("error retrieving volume", err)
	}

	if len(vol.DropletIDs) > 0 {
		if !opts.Force {
			return nil, "", goof.New("volume already attached")
		}

		err = d.volumeDetach(ctx, vol)
		if err != nil {
			return nil, "", err
		}
	}

	dropletID := mustInstanceIDID(ctx)
	dropletIDI, err := strconv.Atoi(*dropletID)
	if err != nil {
		return nil, "", err
	}

	action, _, err := d.client.StorageActions.Attach(
		ctx, volumeID, dropletIDI)
	if err != nil {
		return nil, "", err
	}

	err = d.waitForAction(ctx, volumeID, action)
	if err != nil {
		return nil, "", err
	}

	attachedVol, err := d.VolumeInspect(ctx, volumeID,
		&types.VolumeInspectOpts{
			Attachments: types.VolAttReqTrue},
	)
	if err != nil {
		return nil, "", goof.WithError("error retrieving volume", err)
	}

	return attachedVol, attachedVol.Name, nil
}

func (d *driver) VolumeDetach(
	ctx types.Context, volumeID string,
	opts *types.VolumeDetachOpts) (*types.Volume, error) {
	vol, _, err := d.client.Storage.GetVolume(ctx, volumeID)
	if err != nil {
		return nil, goof.WithError("error getting volume", err)
	}

	if len(vol.DropletIDs) == 0 {
		return nil, goof.WithError("volume already detached", err)
	}

	err = d.volumeDetach(ctx, vol)
	if err != nil {
		return nil, err
	}

	ctx.WithField("volumeID", volumeID).Info("detached volume")

	detachedVol, err := d.VolumeInspect(ctx, volumeID,
		&types.VolumeInspectOpts{
			Attachments: types.VolAttReqTrue,
			Opts:        opts.Opts,
		},
	)
	if err != nil {
		return nil, goof.WithError("error getting volume information",
			err)
	}

	return detachedVol, nil
}

func (d *driver) Snapshots(
	ctx types.Context, opts types.Store) ([]*types.Snapshot, error) {
	return nil, types.ErrNotImplemented
}

func (d *driver) SnapshotInspect(
	ctx types.Context, snapshotID string,
	opts types.Store) (*types.Snapshot, error) {
	return nil, types.ErrNotImplemented
}

func (d *driver) SnapshotCopy(
	ctx types.Context, snapshotID string, snapshotName string, destinationID string,
	opts types.Store) (*types.Snapshot, error) {
	return nil, types.ErrNotImplemented
}

func (d *driver) SnapshotRemove(
	ctx types.Context, snapshotID string, opts types.Store) error {
	return types.ErrNotImplemented
}

func mustInstanceIDID(ctx types.Context) *string {
	return &context.MustInstanceID(ctx).ID
}

func (d *driver) toTypesVolume(
	ctx types.Context,
	volume *godo.Volume,
	attachments types.VolumeAttachmentsTypes) (*types.Volume, error) {

	var (
		ld    *types.LocalDevices
		ldOK  bool
		iidID string
	)

	if attachments.Devices() {
		// Get local devices map from context
		if ld, ldOK = context.LocalDevices(ctx); !ldOK {
			return nil, goof.New(
				"error getting local devices from context")
		}

		// We require the InstanceID to match the VM
		iidID = context.MustInstanceID(ctx).ID
	}

	vol := &types.Volume{
		Name:             volume.Name,
		ID:               volume.ID,
		Encrypted:        false,
		Size:             volume.SizeGigaBytes,
		AvailabilityZone: volume.Region.Slug,
	}

	// Collect attachment info for the volume
	if attachments.Requested() {
		var atts []*types.VolumeAttachment
		for _, id := range volume.DropletIDs {
			strDropletID := strconv.Itoa(id)
			attachment := &types.VolumeAttachment{
				VolumeID: volume.ID,
				InstanceID: &types.InstanceID{
					ID:     strDropletID,
					Driver: d.Name(),
				},
			}

			if attachments.Devices() {
				if strDropletID == iidID {
					if dev, ok := ld.DeviceMap[vol.Name]; ok {
						attachment.DeviceName = dev
					}
				}
			}
			atts = append(atts, attachment)
		}
		if len(atts) > 0 {
			vol.Attachments = atts
		}
	}

	return vol, nil
}

func (d *driver) waitForAction(
	ctx types.Context,
	volumeID string,
	action *godo.Action) error {
	// TODO expose these ints as options
	for i := 0; i < 10; i++ {
		duration := i * 15
		time.Sleep(time.Duration(duration) * time.Millisecond)

		action, _, err := d.client.StorageActions.Get(
			ctx, volumeID, action.ID)
		if err != nil {
			return err
		}
		if action.Status == godo.ActionCompleted {
			break
		}
	}

	return nil
}
