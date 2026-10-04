import type { ChipProps } from "@tscircuit/props"

const pinLabels = {
  pin1: ["pin1"],
  pin2: ["pin2"],
  pin3: ["pin3"],
  pin4: ["pin4"],
  pin5: ["pin5"]
} as const

export const SRD_05VDC_SL_C = (props: ChipProps<typeof pinLabels>) => {
  return (
    <chip
      pinLabels={pinLabels}
      symbol={
        <symbol>
          <schematicrect schX={0} schY={0} width={0.8} height={1.8} strokeWidth={0.02} color="#880000" />
          <port name="pin1" pinNumber={1} aliases={["1"]} direction="right" schX={0.8} schY={-0.5} schStemLength={0.4} />
          <port name="pin2" pinNumber={2} aliases={["2"]} direction="right" schX={0.8} schY={0.5} schStemLength={0.4} />
          <port name="pin3" pinNumber={3} aliases={["3"]} direction="left" schX={-0.8} schY={0.5} schStemLength={0.4} />
          <port name="pin4" pinNumber={4} aliases={["4"]} direction="left" schX={-0.8} schY={-0.5} schStemLength={0.4} />
          <port name="pin5" pinNumber={5} aliases={["5"]} direction="left" schX={-0.8} schY={0.1} schStemLength={0.4} />
          <schematicpath points={[{"x":0.3,"y":-0.5},{"x":0.1,"y":-0.5}]} strokeColor="#880000" />
          <schematicpath points={[{"x":-0.1,"y":-0.5},{"x":-0.3,"y":-0.5}]} strokeColor="#880000" />
          <schematicpath points={[{"x":-0.1,"y":-0.7},{"x":0.1,"y":-0.7},{"x":0.1,"y":-0.3},{"x":-0.1,"y":-0.3},{"x":-0.1,"y":-0.7}]} strokeColor="#880000" />
          <schematicpath svgPath="M -0.3 -0.48 A 0.02 0.02 0 1 0 -0.3 -0.48" strokeColor="#880000" />
          <schematicpath svgPath="M 0.3 -0.48 A 0.02 0.02 0 1 0 0.3 -0.48" strokeColor="#880000" />
          <schematicpath points={[{"x":-0.3,"y":0.1},{"x":-0.06,"y":0.1},{"x":-0.06,"y":0.3}]} strokeColor="#880000" />
          <schematicpath points={[{"x":0.3,"y":0.5},{"x":0.06,"y":0.5}]} strokeColor="#880000" />
          <schematicpath points={[{"x":-0.3,"y":0.5},{"x":-0.16,"y":0.5}]} strokeColor="#880000" />
          <schematicpath points={[{"x":-0.06,"y":0.3},{"x":-0.16,"y":0.6}]} strokeColor="#880000" />
          <schematicpath svgPath="M 0.3 0.52 A 0.02 0.02 0 1 0 0.3 0.52" strokeColor="#880000" />
          <schematicpath svgPath="M -0.3 0.12 A 0.02 0.02 0 1 0 -0.3 0.12" strokeColor="#880000" />
          <schematicpath svgPath="M -0.3 0.52 A 0.02 0.02 0 1 0 -0.3 0.52" strokeColor="#880000" />
        </symbol>
      }
      supplierPartNumbers={{
  "jlcpcb": [
    "C35449"
  ]
}}
      manufacturerPartNumber="SRD-05VDC-SL-C"
      footprint={<footprint>
        <platedhole  portHints={["pin2"]} pcbX="7.100062mm" pcbY="-5.99948mm" outerDiameter="2.794mm" holeDiameter="1.524mm" shape="circle" />
<platedhole  portHints={["pin5"]} pcbX="-7.100062mm" pcbY="0mm" outerDiameter="2.794mm" holeDiameter="1.524mm" shape="circle" />
<platedhole  portHints={["pin4"]} pcbX="-5.100066mm" pcbY="5.999988mm" outerDiameter="2.794mm" holeDiameter="1.524mm" shape="circle" />
<platedhole  portHints={["pin1"]} pcbX="-5.100066mm" pcbY="-5.999988mm" outerDiameter="2.794mm" holeDiameter="1.524mm" shape="circle" />
<platedhole  portHints={["pin3"]} pcbX="7.100062mm" pcbY="5.99948mm" outerDiameter="2.794mm" holeDiameter="1.524mm" shape="circle" />
<silkscreenpath route={[{"x":-2.291994399999993,"y":1.050848799999983},{"x":-2.291994399999993,"y":5.99998800000003}]} />
<silkscreenpath route={[{"x":-2.291994399999993,"y":-5.99998800000003},{"x":-2.291994399999993,"y":-1.0512552000000142}]} />
<silkscreenpath route={[{"x":-2.291994399999993,"y":-5.99996260000006},{"x":-3.3098739999999793,"y":-5.99998800000003}]} />
<silkscreenpath route={[{"x":-2.291994399999993,"y":5.99996260000006},{"x":-3.3098739999999793,"y":5.99998800000003}]} />
<silkscreenpath route={[{"x":7.1683880000000215,"y":-4.257039999999961},{"x":7.1683880000000215,"y":-0.3124199999999746}]} />
<silkscreenpath route={[{"x":6.150102000000004,"y":0.7010399999999777},{"x":7.800594000000018,"y":2.3088599999999815}]} />
<silkscreenpath route={[{"x":-7.031735999999995,"y":1.7999964000000546},{"x":-7.031735999999995,"y":3.727195999999992},{"x":1.6042640000000006,"y":3.727195999999992},{"x":1.6042640000000006,"y":0.727201999999977},{"x":1.628393999999986,"y":0.7030466000000501},{"x":5.644388000000021,"y":0.7030466000000501},{"x":6.152388000000002,"y":0.7030466000000501}]} />
<silkscreenpath route={[{"x":10.501096600000011,"y":-7.799984400000028},{"x":-8.699017400000002,"y":-7.799984400000028}]} />
<silkscreenpath route={[{"x":10.500969600000019,"y":7.800035200000025},{"x":-8.699144399999994,"y":7.800035200000025}]} />
<silkscreenpath route={[{"x":10.500995000000017,"y":-7.800009799999941},{"x":10.500995000000017,"y":7.800009799999941}]} />
<silkscreenpath route={[{"x":7.0992999999999995,"y":4.246880000000033},{"x":7.0992999999999995,"y":2.019300000000044}]} />
<silkscreenpath route={[{"x":-8.699144399999994,"y":7.800035200000025},{"x":-8.699144399999994,"y":0.2946907999999553}]} />
<silkscreenpath route={[{"x":-8.699144399999994,"y":-0.2946907999999553},{"x":-8.699144399999994,"y":-7.799984400000028}]} />
<silkscreencircle pcbX="-7.599934mm" pcbY="-6.800088mm" radius="0.500126mm" />
<silkscreenrect pcbX="-2.274951mm" pcbY="-0mm" width="2.999994mm" height="1.999996mm" strokeWidth="0.254mm" />
<silkscreentext text="{NAME}" pcbX="0.88773mm" pcbY="8.8994mm" anchorAlignment="center" fontSize="1mm" />
<courtyardoutline outline={[{"x":-8.963977999999969,"y":8.154988000000003},{"x":10.735983599999997,"y":8.154988000000003},{"x":10.735983599999997,"y":-8.154988000000003},{"x":-8.963977999999969,"y":-8.154988000000003},{"x":-8.963977999999969,"y":8.154988000000003}]} />
      </footprint>}
      cadModel={{
        objUrl: "https://modelcdn.tscircuit.com/easyeda_models/assets/C35449.obj?uuid=c54660e52b534f81a7bcd3533248af67",
        stepUrl: "https://modelcdn.tscircuit.com/easyeda_models/assets/C35449.step?uuid=c54660e52b534f81a7bcd3533248af67",
        pcbRotationOffset: 0,
        modelOriginPosition: { x: 7.113997199999972, y: 6.205, z: -6.300006000000001 },
      }}
      {...props}
    />
  )
}