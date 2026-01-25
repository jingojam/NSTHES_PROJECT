#compiled build script for gst-plugins-rs
sudo apt update

# HELPER FUNCTIONS
#helper function to check if certain package exists
check_package(){
   #find if package exists
   if dpkg-query -W -f='${Status}' "$1" 2>/dev/null | grep -q "install ok installed"; then
      return 0
   else
      return 1
   fi
}

# helper function to verify package existence and installation
install_package(){
   check_package "$1"
   res=$?
   
   if [ "$res" -eq 0 ]; then
      echo "[$1 found. Skipping installation.]"
   else
      echo "[$1 not found.]"
      echo "[Installing $1.]"
      sudo apt install -y "$1"
   fi
}

#verify dependencies first
echo "------- Verifying Core Dependencies -------"

#package container: put package names here
package_dependencies=(
    "git"
    "curl"
    "build-essential"
    "autoconf"
    "automake"
    "libtool"
    "pkg-config"
    "meson"
    "ninja-build"
    "libglib2.0-dev"
    "libgstreamer1.0-dev"
    "libgstreamer-plugins-base1.0-dev"
    "libgstreamer-plugins-bad1.0-dev"
    "gstreamer1.0-plugins-good"
    "gstreamer1.0-plugins-bad"
    "gstreamer1.0-plugins-ugly"
    "gstreamer1.0-libav"
    "gstreamer1.0-tools"
    "libev-dev"
    "libssl-dev"
    "nasm"
    "libclang-dev"
    "clang"
    "wireshark"
)

# iterate through each package and verify installation
for package in "${package_dependencies[@]}"; do
   install_package "$package"
done

# if rust is not found, install it
if ! command -v rustup &> /dev/null; then
    echo "[Installing rustup]"
    curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y
fi

# update rustup
source "$HOME/.cargo/env"
rustup update stable
rustup default stable

#install cargo-c for Gstreamer
cargo install cargo-c --force

# this should be /home/<user>/
cd "$HOME"

# build and install the gst-plugins-rs
echo "------- Verifying gst-plugins-rs -------"
if [ ! -d "gst-plugins-rs" ]; then #check if gst-plugins-rs repository isn't already present
   echo "[Cloning into https://gitlab.freedesktop.org/gstreamer/gst-plugins-rs.]"
   git clone https://gitlab.freedesktop.org/gstreamer/gst-plugins-rs
else
   echo "[Repository already in $HOME.]"
fi

cd gst-plugins-rs

# we specifically need only the rtp plugin and the quinn (quic/roq) plugin for our purpose
echo "------- Building RTP and QUIC Stacks -------"
cargo cbuild -p gst-plugin-rtp --release
cargo cbuild -p gst-plugin-quinn --release

# insall rtp and quinn plugins
sudo RUSTUP_HOME=$HOME/.rustup CARGO_HOME=$HOME/.cargo $HOME/.cargo/bin/cargo cinstall -p gst-plugin-rtp --prefix=/usr --libdir=/usr/lib/x86_64-linux-gnu
sudo RUSTUP_HOME=$HOME/.rustup CARGO_HOME=$HOME/.cargo $HOME/.cargo/bin/cargo cinstall -p gst-plugin-quinn --prefix=/usr --libdir=/usr/lib/x86_64-linux-gnu

#sudo cargo cinstall -p gst-plugin-rtp --prefix=/usr
#sudo cargo cinstall -p gst-plugin-quinn --prefix=/usr

#clear cache because sometimes Gstreamer isn't able to see plugins due to cache weirdness
rm ~/.cache/gstreamer-1.0/registry.x86_64.bin

# check if present
echo "------- Verification -------"
echo "[RTP elements]"
gst-inspect-1.0 rtp
echo "[QUIC (quinn) elements]"
gst-inspect-1.0 quinn

cd $HOME

# allow execution for roq_sender.sh and roq_receiver.sh
chmod +x roq_sender.sh
chmod +x roq_receiver.sh
