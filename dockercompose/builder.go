package dockercompose

import "strings"

func BuildDockware(dockwareImage string, mt MountType, swVersion string, withMySQL bool, withElastic bool, withRedis bool, withAppServer bool, withPWA bool) (string, error) {

	dc := Create("3.0")
	sw := dc.AddOverwriteService("shopware", "shopware", "dockware/"+dockwareImage+":"+strings.TrimSpace(swVersion))

	switch mt {
	case BindMount:
		sw.AddVolume("./src", "/var/www/html")
	case Volume:
		sw.AddVolume("shop_volume", "/var/www/html")
	}

	sw.AddPorts(map[int]int{80: 80, 443: 443})
	if mt == Sftp {
		sw.AddPort(22, 22)
	}

	if withAppServer {
		as := dc.AddOverwriteService("app", "app", "dockware/flex:latest")
		switch mt {
		case BindMount:
			as.AddVolume("./app", "/var/www/html")
		case Volume:
			as.AddVolume("app_volume", "/var/www/html")
		case Sftp:
			as.AddPort(1022, 22)
		}
		as.AddPort(1000, 80)
		as.AddLink("shopware", "dockware.localhost")
	}

	if withPWA {
		sw.AddEnv("SW_API_ACCESS_KEY", "ADD_YOUR_CUSTOM_ACCESS_KEY_HERE")
		pwa := dc.AddOverwriteService("pwa", "pwa", "dockware/flex:latest")
		switch mt {
		case BindMount:
			pwa.AddVolume("./pwa", "/var/www/html")
		case Volume:
			pwa.AddVolume("./pwa_volume", "/var/www/html")
		case Sftp:
			pwa.AddPort(2022, 22)
		}
		pwa.AddPort(2000, 80)
		pwa.AddLink("shopware", "dockware.pwa.dev")
		pwa.AddEnv("NODE_VERSION", "16")
	}

	if withMySQL {
		db := dc.AddOverwriteService("db", "db", "mysql:5.7")
		db.AddPort(3306, 3306)
		db.AddEnv("MYSQL_ROOT_PASSWORD", "root")
		db.AddEnv("MYSQL_PASSWORD", "root")
		db.AddEnv("MYSQL_DATABASE", "shopware")
		db.AddEnv("TZ", "Europe/Berlin")
	}

	if withElastic {
		el := dc.AddOverwriteService("elastic", "elasticsearch", "elasticsearch/latest")
		el.AddPorts(map[int]int{9200: 9200, 9300: 9300})
		el.AddEnv("discovery.type", "single-node")
		el.AddEnv("ES_JAVA_OPTS", "-Xms512m -Xmx512m")
		el.AddEnv("xpack.security.enabled", "false")
	}

	if withRedis {
		red := dc.AddOverwriteService("redis", "redis", "redis_latest")
		red.AddPort(6379, 6379)
	}

	if mt == Volume {
		dc.AddVolume("shop_volume", "local")
		if withAppServer {
			dc.AddVolume("app_volume", "local")
		}
		if withPWA {
			dc.AddVolume("pwa_volume", "local")
		}
	}

	return dc.ToString()
}
