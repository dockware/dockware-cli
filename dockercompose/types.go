package dockercompose

type ImageType int64

const (
	Play ImageType = iota
	Dev
	Contribute
)

func (it ImageType) String() string {
	switch it {
	case Play:
		return "play"
	case Dev:
		return "dev"
	case Contribute:
		return "contribute"
	default:
		return "unknown"
	}
}

type DevType int64

const (
	Plugin DevType = iota
	Shop
	App
	Headless
)

func (dt DevType) String() string {
	switch dt {
	case Plugin:
		return "Plugin"
	case Shop:
		return "Full Shop"
	case App:
		return "App"
	case Headless:
		return "Headless / PWA"
	default:
		return "unkown"
	}
}

type MountType int64

const (
	BindMount MountType = iota
	Volume
	Sftp
)

func (m MountType) String() string {
	switch m {
	case BindMount:
		return "Docker Bind-Mount"
	case Volume:
		return "Docker Volume"
	case Sftp:
		return "SFTP"
	default:
		return "Unknown"
	}
}
