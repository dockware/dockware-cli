package cmd

import (
	"bufio"
	"fmt"
	"github.com/spf13/cobra"
	"os"
	"os/exec"
	"strings"
)

func init() {
	rootCmd.AddCommand(fooCmd)
}

var fooCmd = &cobra.Command{
	Use:   "creator",
	Short: "Use the interactive dockware creator to build get what you need for today's task",
	Long:  "",
	Run: func(cmd *cobra.Command, args []string) {

		reader := bufio.NewReader(os.Stdin)
		fmt.Println("Dockware Creator")
		fmt.Println("")

		fmt.Println("What do you want to do?")
		fmt.Println("(1) Play around with Shopware")
		fmt.Println("(2) Develop with Shopware")
		fmt.Println("(3) Contribute to Shopware")
		fmt.Print(">> ")

		text, _ := reader.ReadString('\n')
		text = strings.Replace(text, "\n", "", -1)

		fmt.Println("")

		if strings.Compare("1", text) == 0 {

			fmt.Println("Fine, let's just use dockware/play for today!")

			swVersion := askShopwareVersion()

			tmpText := fmt.Sprintf("COOL! Just run the following command in your terminal and enjoy Shopware %s", swVersion)
			startCmd := fmt.Sprintf("docker run --rm --name shopware -p 80:80 -p 443:443 dockware/play:%s", swVersion)
			stopCmd := fmt.Sprintf("docker rm -f shopware")

			fmt.Println("")
			fmt.Println(tmpText)
			fmt.Println("")
			fmt.Println("[START] >> " + startCmd)
			fmt.Println("[STOP] >> " + stopCmd)

			cmd := exec.Command(startCmd)
			res, _ := cmd.CombinedOutput()
			fmt.Println(string(res))

		} else if strings.Compare("2", text) == 0 {

			fmt.Println("HEY DEV! YOU LOOK BRILLIANT TODAY.")
			fmt.Println("What do you want to develop?")
			fmt.Println("(1) Plugin")
			fmt.Println("(2) Full Shop")
			fmt.Println("(3) App")
			fmt.Println("(4) Headless / PWA")
			fmt.Print(">> ")
			devType, _ := reader.ReadString('\n')
			devType = strings.Replace(devType, "\n", "", -1)

			fmt.Println("How do you want to work with your Docker containers?")
			fmt.Println("(1) Docker Bind-Mount")
			fmt.Println("(2) Docker Volume")
			fmt.Println("(3) SFTP")
			fmt.Print(">> ")

			workingType, _ := reader.ReadString('\n')
			workingType = strings.Replace(workingType, "\n", "", -1)

			swVersion := askShopwareVersion()
			withElastic := askYesNo("Add Elasticsearch?")
			withMySQL := askYesNo("Add MySQL?")
			withRedis := askYesNo("Add Redis?")
			withAppServer := (devType == "3");
			withPWA := (devType == "4");

			composeFile := buildCompose("dev", workingType, swVersion, withMySQL, withElastic, withRedis, withAppServer, withPWA)

			f, _ := os.Create("docker-compose.yml")
			defer f.Close()
			f.WriteString(composeFile)

			fmt.Println("File generated: ./docker-compose.yml")
			fmt.Println("You can now use this file to start your Docker containers")

		} else if strings.Compare("3", text) == 0 {
			fmt.Println("Fine, one image to serve your needs...")
		}
	},
}

func askShopwareVersion() string {
	reader := bufio.NewReader(os.Stdin)
	fmt.Println("What Shopware version: ")
	fmt.Print(">> ")

	swVersion, _ := reader.ReadString('\n')
	swVersion = strings.Replace(swVersion, "\n", "", -1)

	return swVersion
}

func askYesNo(text string) bool {
	reader := bufio.NewReader(os.Stdin)
	fmt.Println(text + " (y/n)")
	fmt.Print(">> ")

	answer, _ := reader.ReadString('\n')
	answer = strings.Replace(answer, "\n", "", -1)

	return answer == "y"
}

func buildCompose(dockwareImage string, workingType string, swVersion string, withMySQL bool, withElastic bool, withRedis bool, withAppServer bool, withPWA bool) string {
	text := "version: \"3.0\"\n"
	text = text + "\n"

	text = text + "services:\n"

	text = text + "\n"
	text = text + "  shopware:\n"
	text = text + "    container_name: shopware\n"
	text = text + "    image: dockware/" + dockwareImage + ":" + swVersion + "\n"
	switch workingType {
	case "1":
		text = text + "    volumes:\n"
		text = text + "      - \"./src:/var/www/html\"\n"
	case "2":
		text = text + "    volumes:\n"
		text = text + "      - \"shop_volume:/var/www/html\"\n"
	}
	text = text + "    ports:\n"
	text = text + "      - \"80:80\"\n"
	text = text + "      - \"443:443\"\n"
	if workingType == "3" {
		text = text + "      - \"22:22\"\n"
	}

	if withPWA {
		text = text + "    environment:\n"
		text = text + "      # assign your custom access key here\n"
		text = text + "      - SW_API_ACCESS_KEY=SW_XYZ\n"	
	}


	if withAppServer {
		text = text + "\n"
		text = text + "  app:\n"
		text = text + "    container_name: app\n"
		text = text + "    image: dockware/flex:latest\n"
		switch workingType {
		case "1":
			text = text + "    volumes:\n"
			text = text + "      - \"./app:/var/www/html\"\n"
		case "2":
			text = text + "    volumes:\n"
			text = text + "      - \"app_volume:/var/www/html\"\n"
		}
		text = text + "    ports:\n"
		text = text + "      - \"1000:80\"\n"
		if workingType == "3" {
			text = text + "      - \"1022:22\"\n"
		}
		text = text + "    links:\n"
		text = text + "      # use this as the Shopware domain for the handshake and communication back to the shop\n"
		text = text + "      - shopware:dockware.dev\n"
	}

	if withPWA {
		text = text + "\n"
		text = text + "  pwa:\n"
		text = text + "    container_name: pwa\n"
		text = text + "    image: dockware/flex:latest\n"
		switch workingType {
		case "1":
			text = text + "    volumes:\n"
			text = text + "      - \"./pwa:/var/www/html\"\n"
		case "2":
			text = text + "    volumes:\n"
			text = text + "      - \"pwa_volume:/var/www/html\"\n"
		}
		text = text + "    ports:\n"
		text = text + "      - \"2000:80\"\n"
		if workingType == "3" {
			text = text + "      - \"2022:22\"\n"
		}
		text = text + "    links:\n"
		text = text + "      # use this as the shopwareEndpoint domain\n"
		text = text + "      - shopware:dockware.pwa.dev\n"
		text = text + "    environment:\n"
		text = text + "      - NODE_VERSION=16\n"
	}


	if withMySQL {
		text = text + "\n"
		text = text + "  db:\n"
		text = text + "    container_name: db\n"
		text = text + "    image: mysql:5.7\n"
		text = text + "    ports:\n"
		text = text + "      - \"3306:3306\"\n"
		text = text + "    environment:\n"
		text = text + "      - MYSQL_ROOT_PASSWORD=root\n"
		text = text + "      - MYSQL_PASSWORD=root\n"
		text = text + "      - MYSQL_DATABASE=shopware\n"
		text = text + "      - TZ=Europe/Berlin\n"
	}

	if withElastic {
		text = text + "\n"
		text = text + "  elastic:\n"
		text = text + "    container_name: elasticsearch\n"
		text = text + "    image: elasticsearch/latest\n"
		text = text + "    ports:\n"
		text = text + "      - \"9200:9200\"\n"
		text = text + "      - \"9300:9300\"\n"
		text = text + "    environment:\n"
		text = text + "      - \"discovery.type=single-node\"\n"
		text = text + "      # adjust the memory to your needs\n"
		text = text + "      - \"ES_JAVA_OPTS=-Xms512m -Xmx512m\"\n"
		text = text + "      - \"xpack.security.enabled=false\"\n"
	}

	if withRedis {
		text = text + "\n"
		text = text + "  redis:\n"
		text = text + "    container_name: redis\n"
		text = text + "    image: redis/latest\n"
		text = text + "    ports:\n"
		text = text + "      - \"6379:6379\"\n"
	}

	if workingType == "2" {
		text = text + "\n"
		text = text + "volumes:\n"
		text = text + "  shop_volume:\n"
		text = text + "    driver: local\n"

		if withAppServer {
			text = text + "  app_volume:\n"
			text = text + "    driver: local\n"
		}

		if withPWA {
			text = text + "  pwa_volume:\n"
			text = text + "    driver: local\n"
		}
	}
	return text
}
