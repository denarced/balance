#!/usr/bin/env bash

set -eu

shopt -s globstar
vim ./**/*.go go.*
shopt -u globstar
