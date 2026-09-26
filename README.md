# Grab Bag -- drag and drop files from your terminal

A small file-drag app for Wayland desktops written in Go. It renders using Wayland primitives; no GTK, Qt, CGO, or native development libraries are used or needed.

## Build and run

Building requires Go 1.27.1 or newer:

```sh
git clone https://github.com/SpinningVinyl/GrabBag.git
cd GrabBag
CGO_ENABLED=0 go build -buildvcs=false -ldflags='-s -w' -o grbg .
```

When you run the app without any arguments, it launches in the background and doesn't show its window. `--foreground` keeps the process attached to the terminal for debugging.

To add files to the Grab Bag:
```sh
grbg file1 file2
```

It fully supports stdin, so you can also pipe the output of other commands into it:

```sh
find ~/Pictures -name '*.png' -print | ./grbg
find ~/Pictures -name '*.png' -print0 | ./grbg -0
```

By default, files are added to the bag instead of replacing the old contents. To delete the old contents when adding new files, run:

```sh
grbg --replace file3
```

To toggle the window's visibility:

```sh
grbg --toggle
```

To exit the app:

```sh
grbg --stop
```

## License

Grab Bag is licensed under the GNU General Public License, version 2 or (at
your option) any later version (`GPL-2.0-or-later`). See [LICENSE](LICENSE) for
the full license text.

See [NOTICE](NOTICE) for third party copyright notices and licenses.
