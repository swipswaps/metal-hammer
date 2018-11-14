package cmd

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"git.f-i-ts.de/cloud-native/metal/metal-hammer/metal-core/models"
	"git.f-i-ts.de/cloud-native/metal/metal-hammer/pkg"
	log "github.com/inconshreveable/log15"
	"github.com/mholt/archiver"
	lz4 "github.com/pierrec/lz4"
	pb "gopkg.in/cheggaaa/pb.v1"
	"gopkg.in/yaml.v2"
)

var (
	defaultDisk = Disk{
		Device: "/dev/sda",
		Partitions: []*Partition{
			&Partition{
				Label:      "efi",
				Number:     1,
				MountPoint: "/boot/efi",
				Filesystem: VFAT,
				GPTType:    GPTBoot,
				GPTGuid:    EFISystemPartition,
				Size:       300,
			},
			&Partition{
				Label:      "root",
				Number:     2,
				MountPoint: "/",
				Filesystem: EXT4,
				GPTType:    GPTLinux,
				Size:       -1,
			},
		},
	}
)

const (
	// FAT32 is used for the UEFI boot partition
	FAT32 = FSType("fat32")
	// VFAT is used for the UEFI boot partition
	VFAT = FSType("vfat")
	// EXT3 is usually only used for /boot
	EXT3 = FSType("ext3")
	// EXT4 is the default fs
	EXT4 = FSType("ext4")
	// SWAP is for the swap partition
	SWAP = FSType("swap")

	// GPTBoot EFI Boot Partition
	GPTBoot = GPTType("ef00")
	// GPTLinux Linux Partition
	GPTLinux = GPTType("8300")
	// EFISystemPartition see https://en.wikipedia.org/wiki/EFI_system_partition
	EFISystemPartition = "C12A7328-F81F-11D2-BA4B-00A0C93EC93B"
)

const (
	prefix             = "/rootfs"
	osImageDestination = "/tmp/os.tgz"
)

// GPTType is the GUID Partition table type
type GPTType string

// GPTGuid is the UID of the GPT partition to create
type GPTGuid string

// FSType defines the Filesystem of a Partition
type FSType string

// Partition defines a disk partition
type Partition struct {
	Label        string
	Device       string
	Number       uint
	MountPoint   string
	MountOptions []*MountOption

	// Size in mebiBytes. If negative all available space is used.
	Size       int64
	Filesystem FSType
	GPTType    GPTType
	GPTGuid    GPTGuid
}

func (p *Partition) String() string {
	return fmt.Sprintf("%s", p.Device)
}

// MountOption a option given to a mountpoint
type MountOption string

// Disk is a physical Disk
type Disk struct {
	Device string
	// Partitions to create on this disk, order is preserved
	Partitions []*Partition
}

// InstallerConfig contains configuration items which are
// consumed by the install.sh of the individual target OS.
type InstallerConfig struct {
	Hostname     string `yaml:"hostname"`
	SSHPublicKey string `yaml:"sshpublickey"`
	// is expected to be in the form without mask
	IPAddress string `yaml:"ipaddress"`
	// must be calculated from the last 4 byte of the IPAddress
	ASN string `yaml:"asn"`
}

// init set calculated Device of every partition
func init() {
	for _, p := range defaultDisk.Partitions {
		p.Device = fmt.Sprintf("%s%d", defaultDisk.Device, p.Number)
	}
}

// Wait until a device create request was fired
func (h *Hammer) Wait(uuid string) (*models.ModelsMetalDeviceWithPhoneHomeToken, error) {
	e := fmt.Sprintf("http://%v/device/install/%v", h.Spec.MetalCoreURL, uuid)
	log.Info("waiting for install, long polling", "url", e, "uuid", uuid)

	var resp *http.Response
	var err error
	for {
		resp, err = http.Get(e)
		if err != nil || resp.StatusCode != http.StatusOK {
			log.Warn("wait for install failed, retrying...", "error", err, "statuscode", resp.StatusCode)
		} else {
			break
		}
		time.Sleep(2 * time.Second)
	}

	deviceJSON, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("wait for install reading response failed with: %v", err)
	}

	var deviceWithToken models.ModelsMetalDeviceWithPhoneHomeToken
	err = json.Unmarshal(deviceJSON, &deviceWithToken)
	if err != nil {
		return nil, fmt.Errorf("wait for install could not unmarshal response with error: %v", err)
	}
	log.Info("stopped waiting got", "deviceWithToken", deviceWithToken)

	return &deviceWithToken, nil
}

// Install a given image to the disk by using genuinetools/img
func Install(deviceWithToken *models.ModelsMetalDeviceWithPhoneHomeToken) (*pkg.Bootinfo, error) {
	device := deviceWithToken.Device
	phtoken := deviceWithToken.PhoneHomeToken
	image := *device.Image.URL
	err := partition(defaultDisk)
	if err != nil {
		return nil, err
	}

	err = mountPartitions(prefix, defaultDisk)
	if err != nil {
		return nil, err
	}

	err = pull(image)
	if err != nil {
		return nil, err
	}
	err = burn(prefix, image)
	if err != nil {
		return nil, err
	}

	info, err := install(prefix, device, *phtoken)
	if err != nil {
		return nil, err
	}
	return info, nil
}

func mountPartitions(prefix string, disk Disk) error {
	log.Info("mount disk", "disk", disk)
	// "/" must be mounted first
	partitions := disk.SortByMountPoint()

	for _, p := range partitions {
		err := createFilesystem(p)
		if err != nil {
			log.Error("mount partition create filesystem failed", "error", err)
			return fmt.Errorf("mount partitions create fs failed: %v", err)
		}

		if p.MountPoint == "" {
			continue
		}

		mountPoint := filepath.Join(prefix, p.MountPoint)
		err = os.MkdirAll(mountPoint, os.ModePerm)
		if err != nil {
			log.Error("mount partition create directory", "error", err)
			return fmt.Errorf("mount partitions create directory failed: %v", err)
		}
		log.Info("mount partition", "partition", p.Device, "mountPoint", mountPoint)
		// see man 2 mount
		err = syscall.Mount(p.Device, mountPoint, string(p.Filesystem), 0, "")
		if err != nil {
			log.Error("unable to mount", "partition", p.Device, "mountPoint", mountPoint, "error", err)
			return fmt.Errorf("mount partitions mount: %s to:%s failed: %v", p.Device, mountPoint, err)
		}
	}

	return nil
}

// SortByMountPoint ensures that "/" is the first, which is required for mounting
func (d *Disk) SortByMountPoint() []*Partition {
	ordered := make([]*Partition, 0)
	for _, p := range d.Partitions {
		if p.MountPoint == "/" {
			ordered = append(ordered, p)
		}
	}
	for _, p := range d.Partitions {
		if p.MountPoint != "/" {
			ordered = append(ordered, p)
		}
	}
	return ordered
}

// pull a image from s3
func pull(image string) error {
	log.Info("pull image", "image", image)
	destination := osImageDestination
	md5destination := destination + ".md5"
	md5file := image + ".md5"
	err := download(image, destination)
	if err != nil {
		return fmt.Errorf("unable to pull image %s error: %v", image, err)
	}
	err = download(md5file, md5destination)
	defer os.Remove(md5destination)
	if err != nil {
		return fmt.Errorf("unable to pull md5 %s error: %v", md5file, err)
	}
	log.Info("check md5")
	matches, err := checkMD5(destination, md5destination)
	if err != nil || !matches {
		return fmt.Errorf("md5sum mismatch %v", err)
	}

	log.Info("pull image done", "image", image)
	return nil
}

// burn a image pulling a tarball and unpack to a specific directory
func burn(prefix, image string) error {
	log.Info("burn image", "image", image)
	begin := time.Now()
	source := osImageDestination

	file, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("%s: failed to open archive: %v", source, err)

	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		return fmt.Errorf("unable to stat %s error: %v", source, err)
	}

	if !strings.HasSuffix(image, "lz4") {
		return fmt.Errorf("unsupported image compression format of image:%s", image)
	}

	lz4Reader := lz4.NewReader(file)
	log.Info("lz4", "size", lz4Reader.Header.Size)
	creader := ioutil.NopCloser(lz4Reader)
	// wild guess for lz4 compression ratio
	// lz4 is a stream format and therefore the
	// final size cannot be calculated upfront
	csize := stat.Size() * 2
	defer creader.Close()

	bar := pb.New64(csize).SetUnits(pb.U_BYTES)
	bar.Start()
	bar.SetWidth(80)
	bar.ShowSpeed = true

	reader := bar.NewProxyReader(creader)

	err = archiver.Tar.Read(reader, prefix)
	if err != nil {
		return fmt.Errorf("unable to burn image %s error: %v", source, err)
	}

	bar.Finish()

	err = os.Remove(source)
	if err != nil {
		log.Warn("burn image unable to remove image source", "error", err)
	}

	log.Info("burn took", "duration", time.Since(begin))
	return nil
}

type mount struct {
	source string
	target string
	fstype string
	flags  uintptr
	data   string
}

// install will execute /install.sh in the pulled docker image which was extracted onto disk
// to finish installation e.g. install mbr, grub, write network and filesystem config
func install(prefix string, device *models.ModelsMetalDevice, phoneHomeToken string) (*pkg.Bootinfo, error) {
	log.Info("install image", "image", device.Image.URL)
	mounts := []mount{
		mount{source: "proc", target: "/proc", fstype: "proc", flags: 0, data: ""},
		mount{source: "sys", target: "/sys", fstype: "sysfs", flags: 0, data: ""},
		mount{source: "tmpfs", target: "/tmp", fstype: "tmpfs", flags: 0, data: ""},
		// /dev is a bind mount, a bind mount must have MS_BIND flags set see man 2 mount
		mount{source: "/dev", target: "/dev", fstype: "", flags: syscall.MS_BIND, data: ""},
	}

	for _, m := range mounts {
		err := syscall.Mount(m.source, prefix+m.target, m.fstype, m.flags, m.data)
		if err != nil {
			log.Error("mounting failed", "source", m.source, "target", m.target, "error", err)
		}
	}

	err := writeInstallerConfig(device)
	if err != nil {
		return nil, fmt.Errorf("writing configuration install.yaml failed:%v", err)
	}

	err = writePhoneHomeToken(phoneHomeToken)
	if err != nil {
		return nil, fmt.Errorf("writing phoneHome.jwt failed:%v", err)
	}

	log.Info("running /install.sh on", "prefix", prefix)
	err = os.Chdir(prefix)
	if err != nil {
		return nil, fmt.Errorf("unable to chdir to: %s error:%v", prefix, err)
	}
	cmd := exec.Command("/install.sh")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// these syscalls are required to execute the command in a chroot env.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{
			Uid:    uint32(0),
			Gid:    uint32(0),
			Groups: []uint32{0},
		},
		Chroot: prefix,
	}
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("running install.sh in chroot failed: %v", err)
	}

	err = os.Chdir("/")
	if err != nil {
		return nil, fmt.Errorf("unable to chdir to: / error:%v", err)
	}
	log.Info("finish running /install.sh")

	err = os.Remove(path.Join(prefix, "/install.sh"))
	if err != nil {
		log.Warn("unable to remove install.sh, ignoring...", "error", err)
	}

	info, err := readBootInfo()
	if err != nil {
		return nil, fmt.Errorf("unable to read boot-info.yaml: %v", err)
	}

	files := []string{info.Kernel, info.Initrd}
	tmp := "/tmp"
	for _, f := range files {
		src := path.Join(prefix, f)
		dest := path.Join(tmp, filepath.Base(f))
		_, err := copy(src, dest)
		if err != nil {
			log.Error("could not copy", "src", src, "dest", dest, "error", err)
			return nil, err
		}
	}
	info.Kernel = path.Join(tmp, filepath.Base(info.Kernel))
	info.Initrd = path.Join(tmp, filepath.Base(info.Initrd))

	umounts := [6]string{"/boot/efi", "/proc", "/sys", "/dev", "/tmp", "/"}
	for _, m := range umounts {
		p := prefix + m
		log.Info("unmounting", "mountpoint", p)
		err := syscall.Unmount(p, syscall.MNT_FORCE)
		if err != nil {
			log.Error("unable to umount", "path", p, "error", err)
		}
	}

	return info, nil
}

func writePhoneHomeToken(phoneHomeToken string) error {
	configdir := path.Join(prefix, "etc", "metal")
	destination := path.Join(configdir, "phoneHome.jwt")
	return ioutil.WriteFile(destination, []byte(phoneHomeToken), 0600)
}

func writeInstallerConfig(device *models.ModelsMetalDevice) error {
	log.Info("write installation configuration")
	configdir := path.Join(prefix, "etc", "metal")
	err := os.MkdirAll(configdir, 0755)
	if err != nil {
		return fmt.Errorf("mkdir of %s target os failed: %v", configdir, err)
	}
	destination := path.Join(configdir, "install.yaml")

	var ipaddress string
	var asn int64
	if *device.Cidr == "dhcp" {
		ipaddress = *device.Cidr
	} else {
		ip, _, err := net.ParseCIDR(*device.Cidr)
		if err != nil {
			return fmt.Errorf("unable to parse ip from device.ip: %v", err)
		}

		asn, err = ipToASN(*device.Cidr)
		if err != nil {
			return fmt.Errorf("unable to parse ip from device.ip: %v", err)
		}
		ipaddress = ip.String()
	}

	// FIXME
	sshPubkeys := strings.Join(device.SSHPubKeys, "\n")
	y := &InstallerConfig{
		Hostname:     *device.Hostname,
		SSHPublicKey: sshPubkeys,
		IPAddress:    ipaddress,
		ASN:          fmt.Sprintf("%d", asn),
	}
	yamlContent, err := yaml.Marshal(y)
	if err != nil {
		return err
	}

	return ioutil.WriteFile(destination, yamlContent, 0600)
}

func readBootInfo() (*pkg.Bootinfo, error) {
	bi, err := ioutil.ReadFile(path.Join(prefix, "etc", "metal", "boot-info.yaml"))
	if err != nil {
		return nil, fmt.Errorf("could not read boot-info.yaml: %v", err)
	}

	info := &pkg.Bootinfo{}
	err = yaml.Unmarshal(bi, info)
	if err != nil {
		return nil, fmt.Errorf("could not unmarshal boot-info.yaml: %v", err)
	}
	return info, nil
}
