<div align="center">
    <img alt="JetKVM logo" src="https://jetkvm.com/logo-blue.png" height="28">

### KVM

[Discord](https://jetkvm.com/discord) | [Website](https://jetkvm.com) | [Issues](https://github.com/jetkvm/cloud-api/issues) | [Docs](https://jetkvm.com/docs)

[![Twitter](https://img.shields.io/twitter/url/https/twitter.com/jetkvm.svg?style=social&label=Follow%20%40JetKVM)](https://twitter.com/jetkvm)

[![Go Report Card](https://goreportcard.com/badge/github.com/jetkvm/kvm)](https://goreportcard.com/report/github.com/jetkvm/kvm)

</div>

## WebSocket video transport

This build adds a WebSocket fallback for networks where WebRTC cannot connect at all. When the browser reaches the device through an application-level tunnel (ZTNA/SASE products such as Zscaler), it has no IP route to the device, so ICE can never pair a candidate — signalling succeeds over HTTP while media has nowhere to go. The device therefore also serves video, JSON-RPC and HID over the single client-initiated connection that already loaded the UI (`GET /stream/ws`): H.264 is muxed into fragmented MP4 and played through Media Source Extensions, with no UDP, no second port and no inbound connection. It engages automatically when WebRTC fails, or immediately with `?transport=ws`. Audio, terminal, serial and file upload remain WebRTC-only.

To deploy, first give the device your SSH key — paste `~/.ssh/id_ed25519.pub` (create one with `ssh-keygen -t ed25519`) into Settings → Advanced → Developer Mode in the web UI, since `dev_deploy.sh` opens about fifteen separate SSH connections and would otherwise prompt for a password each time. For a temporary test run, `./dev_deploy.sh -r <device-ip> --skip-native-build` builds and runs the binary in the foreground with logs streaming to your terminal; Ctrl-C stops it and a reboot restores the stock app, so nothing on the device is overwritten. To install permanently, `./dev_deploy.sh -r <device-ip> -i` builds a release binary, stages it exactly as the OTA updater would, and reboots the device.

JetKVM is a high-performance, open-source KVM over IP (Keyboard, Video, Mouse) solution designed for efficient remote management of computers, servers, and workstations. Whether you're dealing with boot failures, installing a new operating system, adjusting BIOS settings, or simply taking control of a machine from afar, JetKVM provides the tools to get it done effectively.

## Features

- **Ultra-low Latency** - 1080p@60FPS video with 30-60ms latency using H.264 encoding. Smooth mouse and keyboard interaction for responsive remote control.
- **Free & Optional Remote Access** - Remote management via JetKVM Cloud using WebRTC.
- **Optional Tailscale Networking** - Built-in Tailscale status and control-server configuration, including custom [Headscale](https://headscale.net/)-compatible endpoints.
- **Open-source software** - Written in Golang on Linux. Easily customizable through SSH access to the JetKVM device.

## Contributing

We welcome contributions from the community! Whether it's improving the firmware, adding new features, or enhancing documentation, your input is valuable. We also have some rules and taboos here, so please read this page and our [Code of Conduct](/CODE_OF_CONDUCT.md) carefully.

## I need help

The best place to search for answers is our [Documentation](https://jetkvm.com/docs). If you can't find the answer there, check our [Discord Server](https://jetkvm.com/discord).

## I want to report an issue

If you've found an issue and want to report it, please check our [Issues](https://github.com/jetkvm/kvm/issues) page. Make sure the description contains information about the firmware version you're using, your platform, and a clear explanation of the steps to reproduce the issue.

# Development

JetKVM is written in Go & TypeScript. with some bits and pieces written in C. An intermediate level of Go & TypeScript knowledge is recommended for comfortable programming.

The project contains two main parts, the backend software that runs on the KVM device and the frontend software that is served by the KVM device, and also the cloud.

For comprehensive development information, including setup, testing, debugging, and contribution guidelines, see **[DEVELOPMENT.md](DEVELOPMENT.md)**.

For quick device development, use the `./dev_deploy.sh` script. It will build the frontend and backend and deploy them to the local KVM device. Run `./dev_deploy.sh --help` for more information.

## Backend

The backend is written in Go and is responsible for the KVM device management, the cloud API and the cloud web.

## Frontend

The frontend is written in React and TypeScript and is served by the KVM device. It has three build targets: `device`, `development` and `production`. Development is used for development of the cloud version on your local machine, device is used for building the frontend for the KVM device and production is used for building the frontend for the cloud.
