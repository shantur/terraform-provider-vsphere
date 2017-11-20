package vsphere

import (
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"

	"github.com/hashicorp/terraform/terraform"
	"github.com/terraform-providers/terraform-provider-vsphere/vsphere/internal/helper/virtualmachine"
	"github.com/terraform-providers/terraform-provider-vsphere/vsphere/internal/virtualdevice"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/types"
)

// resourceVSphereVirtualMachineMigrateState is the master state migration function for
// the vsphere_virtual_machine resource.
func resourceVSphereVirtualMachineMigrateState(version int, os *terraform.InstanceState, meta interface{}) (*terraform.InstanceState, error) {
	// Guard against a nil state.
	if os == nil {
		return nil, nil
	}

	// Guard against empty state, can't do anything with it
	if os.Empty() {
		return os, nil
	}

	var migrateFunc func(*terraform.InstanceState, interface{}) error
	switch version {
	case 1:
		log.Printf("[DEBUG] Migrating vsphere_virtual_machine state: old v%d state: %#v", version, os)
		migrateFunc = migrateVSphereVirtualMachineStateV2
	case 0:
		log.Printf("[DEBUG] Migrating vsphere_virtual_machine state: old v%d state: %#v", version, os)
		migrateFunc = migrateVSphereVirtualMachineStateV1
	default:
		// Migration is complete
		log.Printf("[DEBUG] Migrating vsphere_virtual_machine state: completed v%d state: %#v", version, os)
		return os, nil
	}
	if err := migrateFunc(os, meta); err != nil {
		return nil, err
	}
	version++
	log.Printf("[DEBUG] Migrating vsphere_virtual_machine state: new v%d state: %#v", version, os)
	return resourceVSphereVirtualMachineMigrateState(version, os, meta)
}

// migrateVSphereVirtualMachineStateV2 migrates the state of the
// vsphere_virtual_machine from version 1 to version 2.
func migrateVSphereVirtualMachineStateV2(is *terraform.InstanceState, meta interface{}) error {
	// All we really preserve from the old state is the UUID of the virtual
	// machine. We leverage some of the special parts of the import functionality
	// - namely validating disks, and flagging the VM as imported in the state to
	// guard against someone adding customization to the configuration and
	// accidentally forcing a new resource.
	//
	// Read will handle most of the population post-migration as it does for
	// import, and there will be an unavoidable diff for TF-only options on the
	// next plan.
	client := meta.(*VSphereClient).vimClient
	name := is.ID
	id := is.Attributes["uuid"]
	if id == "" {
		return fmt.Errorf("resource ID %s has no UUID. State cannot be migrated", name)
	}

	log.Printf("[DEBUG] Migrate state for VM resource %q: UUID %q", name, id)
	vm, err := virtualmachine.FromUUID(client, id)
	if err != nil {
		return fmt.Errorf("error fetching virtual machine: %s", err)
	}
	props, err := virtualmachine.Properties(vm)
	if err != nil {
		return fmt.Errorf("error fetching virtual machine properties: %s", err)
	}

	// Validate the disks in the VM to make sure that they will work with the new
	// version of the resource. This is mainly ensuring that all disks are SCSI
	// disks, but a Read operation is attempted as well to make sure it will
	// survive that.
	//
	// NOTE: This uses the current version of the resource to make this check,
	// which at some point in time may end up being a higher schema version than
	// version 2. At this point in time, there is nothing here that would cause
	// issues (nothing in the sub-resource read logic is reliant on schema
	// versions), and an empty ResourceData is sent anyway.
	diskCnt, _ := strconv.Atoi(is.Attributes["disk.#"])
	maxBus := diskCnt / 15
	l := object.VirtualDeviceList(props.Config.Hardware.Device)
	for k, v := range is.Attributes {
		if !regexp.MustCompile("disk\\.[0-9]+\\.key").MatchString(k) {
			continue
		}
		key, _ := strconv.Atoi(v)
		if key < 1 {
			continue
		}
		device := l.FindByKey(int32(key))
		if device == nil {
			continue
		}
		ctlr := l.FindByKey(device.GetVirtualDevice().ControllerKey)
		if ctlr == nil {
			continue
		}
		if sc, ok := ctlr.(types.BaseVirtualSCSIController); ok && sc.GetVirtualSCSIController().BusNumber > int32(maxBus) {
			maxBus = int(sc.GetVirtualSCSIController().BusNumber)
		}
	}

	d := resourceVSphereVirtualMachine().Data(&terraform.InstanceState{})
	d.Set("scsi_controller_count", maxBus+1)
	if err := virtualdevice.DiskImportOperation(d, client, object.VirtualDeviceList(props.Config.Hardware.Device)); err != nil {
		return err
	}

	rs := resourceVSphereVirtualMachine().Schema
	var guestNetTimeout string
	switch is.Attributes["wait_for_guest_net"] {
	case "false":
		guestNetTimeout = "-1"
	default:
		guestNetTimeout = fmt.Sprintf("%v", rs["wait_for_guest_net_timeout"].Default)
	}

	// The VM should be ready for reading now
	is.Attributes = make(map[string]string)
	is.ID = id
	is.Attributes["imported"] = "true"

	// Set some defaults. This helps possibly prevent diffs where these values
	// have not been changed.
	is.Attributes["scsi_controller_count"] = fmt.Sprintf("%v", rs["scsi_controller_count"].Default)
	is.Attributes["force_power_off"] = fmt.Sprintf("%v", rs["force_power_off"].Default)
	is.Attributes["migrate_wait_timeout"] = fmt.Sprintf("%v", rs["migrate_wait_timeout"].Default)
	is.Attributes["shutdown_wait_timeout"] = fmt.Sprintf("%v", rs["shutdown_wait_timeout"].Default)
	is.Attributes["wait_for_guest_net_timeout"] = guestNetTimeout
	is.Attributes["scsi_controller_count"] = fmt.Sprintf("%v", maxBus+1)

	log.Printf("[DEBUG] %s: Migration complete, resource is ready for read", resourceVSphereVirtualMachineIDString(d))
	return nil
}

func migrateVSphereVirtualMachineStateV1(is *terraform.InstanceState, meta interface{}) error {
	if is.Empty() || is.Attributes == nil {
		log.Println("[DEBUG] Empty VSphere Virtual Machine State; nothing to migrate.")
		return nil
	}

	log.Printf("[DEBUG] Attributes before migration: %#v", is.Attributes)

	if is.Attributes["skip_customization"] == "" {
		is.Attributes["skip_customization"] = "false"
	}

	if is.Attributes["enable_disk_uuid"] == "" {
		is.Attributes["enable_disk_uuid"] = "false"
	}

	for k := range is.Attributes {
		if strings.HasPrefix(k, "disk.") && strings.HasSuffix(k, ".size") {
			diskParts := strings.Split(k, ".")
			if len(diskParts) != 3 {
				continue
			}
			s := strings.Join([]string{diskParts[0], diskParts[1], "controller_type"}, ".")
			if _, ok := is.Attributes[s]; !ok {
				is.Attributes[s] = "scsi"
			}
		}
	}

	log.Printf("[DEBUG] Attributes after migration: %#v", is.Attributes)
	return nil
}
