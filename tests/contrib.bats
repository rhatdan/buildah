#!/usr/bin/env bats

load helpers

@test "verify contrib/buildahimage/stable image shadow-utils is installed correctly" {
  run_buildah bud -t buildah ../contrib/buildahimage/stable
  run_buildah from buildah
  run_buildah run $output rpm -qV shadow-utils
  expect_output "" "rpm -qV shadow-utils should show nothing"
}
