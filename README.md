# dockware-cli


![Shopware 6 Preview](./readme-media/header.jpg)


Welcome to the dockware-cli project.
This is all about a tool to get started with dockware and Shopware even faster.

At the moment we are in an experimental development phase - feel free to share ideas :)
And yes, it's by far not the best code, haha....whatever.


And thank you for using dockware!

## Installation

Just download the binary that you need, and install it as described on our website:
https://dockware.io/cli


## Version
Get the version of your dockware-cli binary.

```bash
dockware-cli version 
```


## Creator
The creator is a tool that helps you to build your Docker setup in an interactive way.
It will guide you through all these questions on what and how you want to develop.

The final result might be a single line to start a Shopware version, or even a full docker-compose.yml file 
with all kinds of containers and settings, ready to be used.

And as always, we just try to help you with the onboarding steps to Docker and stick with the standard.

```bash
dockware-cli creator 
```

The output should then look like this. Just continue answering the questions and you'll end up with a nice docker-compose.yml file.

```bash 
Dockware Creator

? What do you want to do?  [Use arrows to move, type to filter]
> play - A Shopware shop without development tools
  dev - Suited for extension development
  contribute - Best for contributing changes to the Shopware core

...
```


## Purge System
We have provided a command to simply remove all dockware/* images from your system.
Just use this command:

```bash
dockware-cli purge 
```

