# Spec

I want you to build a central portal for all my devices connected through
zerotier to share clipboard and files. There are at least 4 devices in this
network, and they can only connect by using the IP addresses managed by the
tunnel.

THe stack is frontend in simple Javscript and backend with Go. There should
also be a cli client that can communicate to backend through the tunnel. Use
minimal number of dependencies for both frontend and backend. Tell me to
install any tools that you need if they are missing. I would approve them or
sugegst alternatives.

## Backend

It should have one central registry for "clipbaord" and one central registry
for "file". Any upload from client will overwrite the central registry. Client
on request should be able to download the content from registry. The clipboard
registry should be persisted through server machine reboot.

The clipboard registry holds either UTF-8 text or one raster image (PNG, JPEG,
WebP, GIF). Uploads declare their media type and the server stores it; SVG and
other active/markup types are rejected so registry content can never execute in
the portal origin. Uploading text replaces a stored image and vice versa.

The server this runs on is the host.

## Frontend

The frontend should target portrait phone layout only. It should be made into a
PWA. My previous work with very limited functionality may be used as a
reference, but do not copy the UI.

The clipboard card shows text in an editable area and images as a preview. The
Paste action accepts text or an image from the phone clipboard; Copy restores
the current registry content (images are copied as PNG, converting locally when
the stored image is JPEG/WebP/GIF).

The UI should be minimal, with the least amount of buttons doing all the
functionality.

There should only be dark theme and no light theme.

Try to make https work without me paying for SSL certs. It should ideally just
connects without pressing through "trusting this site" browser warning pages.

You may choose to build either single page app or multiple page app, but I want
the least amount of loading.

## CLI

The CLI is used to set and download registry. It runs on a separate Wayland
machine. The CLI should be able to

- read from clipbaord and upload the content to clipboard registry
- download content from clipboard registry and overwrite wayland clipboard
- handle both text and image clipboard content end to end, preserving the MIME
  type (prefer PNG, then JPEG/WebP/GIF, for images read from Wayland)
- upload file
- download file to ~/Downloads or specified location
- All downloading and uploading should show progress bar

The host (this machine) does not have system clipboard. Therefore any clipboard
operation through the CLI should fail safely., Just only consider wayland for
clipboard
