package cmd

import (
	"fmt"
	"github.com/AlecAivazis/survey/v2"
	dc "github.com/dockware/dockware-cli/dockercompose"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

type Answers struct {
	DevIntent       int    `survey:"image"` // setting int as type will yield the index instead of the option
	DevIntentString string `survey:"image"` // is added manually via the image title
	SwVersion       string `survey:"swVersion"`
	DevType         int    `survey:"devType"` // What kind of development will the user do
	MountType       int    `survey:"mountType"`
}

func init() {
	rootCmd.AddCommand(creatorCmd)
}

var creatorCmd = &cobra.Command{
	Use:   "creator",
	Short: "Use the interactive dockware creator to get what you need for today's task",
	Long:  "",
	Run: func(cmd *cobra.Command, args []string) {
		if !term.IsTerminal(int(syscall.Stdin)) {
			fmt.Println("interactive terminal required")
			os.Exit(1)
		}

		a := &Answers{}
		a.getDevIntent()

		switch dc.DevIntent(a.DevIntent) {
		case dc.Play:

			a.SwVersion = askShopwareVersion(dc.Play.String())
			runArgs := []string{"run", "--rm", "--name shopware", "-p 80:80", "-p 443:443", fmt.Sprintf("dockware/%s:%s", a.DevIntentString, a.SwVersion)}

			fmt.Printf("All done! Just run the following command in your terminal and enjoy Shopware %s:\n\n", a.SwVersion)
			fmt.Printf("docker %s\n\n", strings.Join(runArgs, " "))
			execute := false
			err := survey.AskOne(&survey.Confirm{
				Message: "Run the command now?",
			}, &execute)
			if err != nil {
				fmt.Printf("%s\n", err.Error())
				os.Exit(1)
			}
			if execute {
				cmd := exec.Command("docker", runArgs...)
				cmd.Stdin = os.Stdin
				cmd.Stderr = os.Stderr
				cmd.Stdout = os.Stdout
				err := cmd.Run()
				if err != nil {
					fmt.Println(err.Error())
					os.Exit(1)
				}
			}
			return

		case dc.Dev:

			a.getDevType()
			a.getMountType()

			swVersion := askShopwareVersion(a.DevIntentString)
			withElastic := askYesNo("Add Elasticsearch?")
			withMySQL := askYesNo("Add MySQL?")
			withRedis := askYesNo("Add Redis?")
			withAppServer := dc.DevType(a.DevType) == dc.App
			withPWA := dc.DevType(a.DevType) == dc.Headless

			composeString, err := dc.BuildDockware("dev", dc.MountType(a.MountType), swVersion, withMySQL, withElastic, withRedis, withAppServer, withPWA)
			if err != nil {
				fmt.Printf("could not build YAML: %s\n", err.Error())
				os.Exit(1)
			}

			err = os.WriteFile("docker-compose.yml", []byte(composeString), 0666)
			if err != nil {
				fmt.Printf("could not write file: %s\n", err.Error())
				os.Exit(1)
			}

			fmt.Println("File generated: ./docker-compose.yml")
			fmt.Println("You can now use this file to start your Docker containers")
			return
		case dc.Contribute:

			a.getMountType()

			composeString, err := dc.BuildDockware("contribute", dc.MountType(a.MountType), "latest", false, false, false, false, false)
			if err != nil {
				fmt.Errorf("could not build YAML: %s\n", err.Error())
			}

			err = os.WriteFile("docker-compose.yml", []byte(composeString), 0666)
			if err != nil {
				fmt.Printf("could not write file: %s\n", err.Error())
				os.Exit(1)
			}

			fmt.Println("File generated: ./docker-compose.yml")
			fmt.Println("You can now use this file to start your Docker containers")
			return
		}
	},
}

func (a *Answers) getDevType() {
	devTypes := []string{
		dc.Plugin:   "The classic way to extend Shopware",
		dc.Shop:     "A full shop", // TODO better description
		dc.App:      "The new way of extending Shopware",
		dc.Headless: "If you don't want to use the Shopware frontend",
	}
	devTypeTitles := make([]string, len(devTypes))
	for i, _ := range devTypes {
		dt := dc.DevType(i)
		devTypeTitles[i] = dt.String()
	}
	devTypeQuestion := []*survey.Question{
		{
			Name: "devType",
			Prompt: &survey.Select{
				Message: "What do you want to develop?",
				Options: devTypeTitles,
				Description: func(v string, i int) string {
					return devTypes[i]
				},
			},
			Validate: survey.Required,
		},
	}

	err := survey.Ask(devTypeQuestion, a)
	if err != nil {
		fmt.Println(err.Error())
		os.Exit(1)
	}
}

func (a *Answers) getMountType() {
	mountTypes := []string{
		dc.BindMount: dc.BindMount.String(),
		dc.Volume:    dc.Volume.String(),
		dc.Sftp:      dc.Sftp.String(),
	}
	mountTypeTitles := make([]string, len(mountTypes))
	for i, _ := range mountTypes {
		mt := dc.MountType(i)
		mountTypeTitles[i] = mt.String()
	}
	asMountType := []*survey.Question{
		{
			Name: "mountType",
			Prompt: &survey.Select{
				Message: "How do you want to work with your Docker containers?",
				Options: mountTypeTitles,
				Description: func(v string, i int) string {
					return mountTypes[i]
				},
			},
			Validate: survey.Required,
		},
	}
	err := survey.Ask(asMountType, a)
	if err != nil {
		fmt.Println(err.Error())
		os.Exit(1)
	}
}

func (a *Answers) getDevIntent() {
	images := []string{
		dc.Play:       "A Shopware shop without development tools",
		dc.Dev:        "Suited for extension development",
		dc.Contribute: "Best for contributing changes to the Shopware core",
	}
	imageTitles := make([]string, len(images))
	for i, _ := range images {
		it := dc.DevIntent(i)
		imageTitles[i] = it.String()
	}
	askImage := []*survey.Question{
		{
			Name: "image",
			Prompt: &survey.Select{
				Message: "What do you want to do?",
				Options: imageTitles,
				Description: func(v string, i int) string {
					return images[i]
				},
			},
			Validate: survey.Required,
		},
	}
	err := survey.Ask(askImage, a)
	if err != nil {
		fmt.Printf("Could not get Dev Intent: %s\n", err.Error())
		os.Exit(1)
	}
	a.DevIntentString = dc.DevIntent(a.DevIntent).String()
}

func askShopwareVersion(image string) string {
	swVersion := ""
	prompt := &survey.Input{
		Message: "Which shopware version to use?",
		Default: "latest",
		Help:    fmt.Sprintf("For a list of tags see https://hub.docker.com/r/dockware/%s/tags", image),
	}
	err := survey.AskOne(prompt, &swVersion)
	if err != nil {
		return "latest"
	}
	return swVersion
}

func askYesNo(text string) bool {
	result := false
	prompt := &survey.Confirm{
		Message: text,
	}
	err := survey.AskOne(prompt, &result)
	if err != nil {
		return false
	}
	return result
}
