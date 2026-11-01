# filebox

A home file server for an old PC: SFTP for the family, a phone-friendly web portal, and an admin panel that is only reachable over Tailscale.

Install and update guide: [docs/index.html](docs/index.html) (open it in a browser).

Optional TV add-on ([streambox](https://github.com/Ezomic/streambox)): turns your own streams and video files into channels for IPTV apps. Choose it in the Windows wizard (`-Streambox`) or run `ADDONS=streambox ./setup.sh` on the box. See the TV section of the guide.

Quick update on a running box: `sudo filebox-update`. Roll back with `sudo filebox-update rollback`.
