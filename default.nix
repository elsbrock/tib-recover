{
  buildGoModule,
}:

buildGoModule {
  pname = "tib-recover";
  version = "0.1.0";

  src = ./.;
  vendorHash = null;

  meta = {
    description = "Read-only parser for legacy Acronis True Image TIB archives";
    mainProgram = "tib-recover";
  };
}
