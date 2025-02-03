using Microsoft.AspNetCore.Mvc;
using Microsoft.Extensions.ML;
using Microsoft.ML.Data;
using Newtonsoft.Json;
using Prediction_Server;

namespace prediction.Server.Controllers;

[ApiController]
[Route("api/[controller]")]
public class InputDataController : ControllerBase
{
    
    private readonly ILogger<InputDataController> _logger;

    private readonly PredictionEnginePool<PredictionModel.ModelInput, PredictionModel.ModelOutput> _predictionEnginePool;
    private static PredictionModel.ModelInput? inputData;
    public InputDataController(ILogger<InputDataController> logger, PredictionEnginePool<PredictionModel.ModelInput, PredictionModel.ModelOutput> predictionEnginePool)
    {
        _logger = logger;
        predictionEnginePool = _predictionEnginePool;
     
    }

    [HttpGet(Name ="GetInputData")]
    public IActionResult GetInput()
    {
        if (inputData == null)
        {
            return NotFound("input data empty");
        }

        var data = JsonConvert.SerializeObject(inputData, Formatting.Indented);
        return Ok(data);
    }

    
    [HttpPost(Name = "PostInputData")]
    public IActionResult input([FromBody] PredictionModel.ModelInput input)

    {

        if (input == null)
        {
            return BadRequest("Input data is required.");
        }

        inputData = input;

        var data = JsonConvert.SerializeObject(input, Formatting.Indented);

        Console.WriteLine("input" + data);

        return Ok(data);
    }






}
 